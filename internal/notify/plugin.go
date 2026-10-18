package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// 通知插件是运营显式启用（-notify-plugins）后才会加载的本地可执行文件。
// 边界：
//   - 只扫描目录顶层的 *.json 清单，不能覆盖内置通道类型
//   - command 必须是插件目录内的普通可执行文件，符号链接越界一律拒绝
//   - 通过 stdin 传 JSON，不经过 shell；stdout/stderr 有上限
//   - 子进程环境完全重建，不继承服务端环境变量（避免泄露管理令牌等）
//   - 每次调用有 1–30 秒硬超时
const (
	maxPluginManifest = 64 << 10
	maxPluginFields   = 16
	maxPluginArgs     = 32
	maxPluginEnv      = 16
	pluginOutputLimit = 8 << 10
)

var (
	pluginTypeRe  = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,31}$`)
	pluginFieldRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,31}$`)
	pluginEnvRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

type pluginManifest struct {
	Type    string   `json:"type"`
	Label   string   `json:"label"`
	Desc    string   `json:"desc"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Timeout int      `json:"timeout"` // 秒；缺省 10
	Fields  []Field  `json:"fields"`
	Env     []string `json:"env"` // 形如 KEY=VALUE
}

type plugin struct {
	def     ChannelDef
	path    string
	args    []string
	timeout time.Duration
	env     []string
	dir     string
}

var (
	pluginMu   sync.RWMutex
	pluginDefs = map[string]*plugin{}
)

// PluginCount 返回当前已加载的插件数量。
func PluginCount() int {
	pluginMu.RLock()
	defer pluginMu.RUnlock()
	return len(pluginDefs)
}

// LoadPlugins 加载插件目录；空目录参数会清空已加载插件。
// 任一清单非法都会整体失败：显式开启的功能不应带着半个插件集启动。
func LoadPlugins(dir string) error {
	if strings.TrimSpace(dir) == "" {
		pluginMu.Lock()
		pluginDefs = map[string]*plugin{}
		pluginMu.Unlock()
		return nil
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("插件目录无效: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("插件目录不可用: %w", err)
	}
	fi, err := os.Stat(root)
	if err != nil || !fi.IsDir() {
		return fmt.Errorf("插件目录不存在或不是目录: %s", root)
	}
	// 全局可写目录意味着任意本地用户都能替换插件；显式开启也不应接受。
	if fi.Mode().Perm()&0o002 != 0 {
		return fmt.Errorf("插件目录不能是全局可写: %s", root)
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("读取插件目录: %w", err)
	}
	loaded := make(map[string]*plugin)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			continue
		}
		p, err := loadPluginFile(root, filepath.Join(root, e.Name()))
		if err != nil {
			return fmt.Errorf("通知插件 %s: %w", e.Name(), err)
		}
		if _, ok := builtinDefOf(p.def.Type); ok {
			return fmt.Errorf("通知插件 %s: 类型 %q 与内置通道冲突", e.Name(), p.def.Type)
		}
		if _, dup := loaded[p.def.Type]; dup {
			return fmt.Errorf("通知插件 %s: 类型 %q 重复", e.Name(), p.def.Type)
		}
		loaded[p.def.Type] = p
	}

	pluginMu.Lock()
	pluginDefs = loaded
	pluginMu.Unlock()
	return nil
}

// normalizeField 校验并规范化单个插件字段。
//
// type=password 本身就是敏感字段：清单漏写 "secret": true 时
// Config.Redacted() 不会脱敏，密钥会明文下发前端或写入日志，
// 因此在这里强制标记。
func normalizeField(idx int, f Field) (Field, error) {
	f.Key = strings.TrimSpace(f.Key)
	f.Label = strings.TrimSpace(f.Label)
	if !pluginFieldRe.MatchString(f.Key) {
		return f, fmt.Errorf("第 %d 个字段 key 无效", idx+1)
	}
	if f.Label == "" {
		return f, fmt.Errorf("字段 %s 缺少 label", f.Key)
	}
	// 与顶层 label 保持一致的上限：字段 label 会原样透出到前端表单，
	// 不加限制时单个字段就能接近整个清单的大小上限。
	if len([]rune(f.Label)) > 40 {
		return f, fmt.Errorf("字段 %s 的 label 不超过 40 个字符", f.Key)
	}
	switch f.Type {
	case "", "text":
		f.Type = "text"
	case "password":
		f.Type = "password"
		f.Secret = true
	case "number", "url":
	default:
		return f, fmt.Errorf("字段 %s 的 type 无效: %s", f.Key, f.Type)
	}
	if len([]rune(f.Placeholder)) > 200 || len([]rune(f.Hint)) > 200 || len(f.Default) > 128 {
		return f, fmt.Errorf("字段 %s 的提示文本过长", f.Key)
	}
	return f, nil
}

func loadPluginFile(root, path string) (*plugin, error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("清单不可读: %w", err)
	}
	if !insideDir(root, real) {
		return nil, errors.New("清单越出插件目录")
	}
	st, err := os.Stat(real)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > maxPluginManifest {
		return nil, fmt.Errorf("清单必须是小于 %dKB 的普通文件", maxPluginManifest>>10)
	}
	if st.Mode().Perm()&0o002 != 0 {
		return nil, errors.New("清单不能是全局可写")
	}
	b, err := os.ReadFile(real)
	if err != nil {
		return nil, err
	}
	var m pluginManifest
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("清单格式错误: %w", err)
	}

	if !pluginTypeRe.MatchString(m.Type) {
		return nil, fmt.Errorf("type 必须是 2–32 位小写字母/数字/下划线/连字符，且以字母开头")
	}
	m.Label = strings.TrimSpace(m.Label)
	if m.Label == "" || len([]rune(m.Label)) > 40 {
		return nil, errors.New("label 必填且不超过 40 个字符")
	}
	m.Desc = strings.TrimSpace(m.Desc)
	if len([]rune(m.Desc)) > 200 {
		return nil, errors.New("desc 不超过 200 个字符")
	}
	if len(m.Fields) > maxPluginFields {
		return nil, fmt.Errorf("fields 不能超过 %d 个", maxPluginFields)
	}
	seen := map[string]bool{}
	fields := make([]Field, 0, len(m.Fields))
	for i, f := range m.Fields {
		nf, err := normalizeField(i, f)
		if err != nil {
			return nil, err
		}
		if seen[nf.Key] {
			return nil, fmt.Errorf("字段 key 重复: %s", nf.Key)
		}
		seen[nf.Key] = true
		fields = append(fields, nf)
	}

	cmdPath, err := resolvePluginCommand(root, m.Command)
	if err != nil {
		return nil, err
	}
	if len(m.Args) > maxPluginArgs {
		return nil, fmt.Errorf("args 不能超过 %d 个", maxPluginArgs)
	}
	for _, a := range m.Args {
		if len(a) > 512 || strings.ContainsRune(a, 0) {
			return nil, errors.New("args 参数过长或包含 NUL")
		}
	}
	if m.Timeout == 0 {
		m.Timeout = 10
	}
	if m.Timeout < 1 || m.Timeout > 30 {
		return nil, errors.New("timeout 必须在 1–30 秒之间")
	}
	if len(m.Env) > maxPluginEnv {
		return nil, fmt.Errorf("env 不能超过 %d 项", maxPluginEnv)
	}
	env := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"LANG=C.UTF-8",
		"HOME=" + root,
	}
	for _, kv := range m.Env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !pluginEnvRe.MatchString(k) || len(v) > 512 || strings.ContainsRune(v, 0) {
			return nil, fmt.Errorf("env 项无效: %q", kv)
		}
		up := strings.ToUpper(k)
		if up == "PATH" || strings.HasPrefix(up, "LD_") || strings.HasPrefix(up, "DYLD_") ||
			up == "BASH_ENV" || up == "ENV" || up == "SHELLOPTS" {
			return nil, fmt.Errorf("env 不允许覆盖 %s", k)
		}
		env = append(env, k+"="+v)
	}

	return &plugin{
		def: ChannelDef{
			Type:   m.Type,
			Label:  m.Label,
			Desc:   m.Desc,
			Fields: fields,
		},
		path:    cmdPath,
		args:    m.Args,
		timeout: time.Duration(m.Timeout) * time.Second,
		env:     env,
		dir:     root,
	}, nil
}

// resolvePluginCommand 拒绝绝对路径、..、目录逃逸和符号链接逃逸。
func resolvePluginCommand(root, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || filepath.IsAbs(raw) {
		return "", errors.New("command 必须是插件目录内的相对路径")
	}
	clean := filepath.Clean(raw)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", errors.New("command 越出插件目录")
	}
	real, err := filepath.EvalSymlinks(filepath.Join(root, clean))
	if err != nil {
		return "", fmt.Errorf("command 不可用: %w", err)
	}
	if !insideDir(root, real) {
		return "", errors.New("command 通过符号链接越出插件目录")
	}
	fi, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", errors.New("command 必须是普通文件")
	}
	if fi.Mode().Perm()&0o111 == 0 {
		return "", errors.New("command 缺少可执行权限")
	}
	if fi.Mode().Perm()&0o002 != 0 {
		return "", errors.New("command 不能是全局可写")
	}
	return real, nil
}

func insideDir(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func pluginOf(t string) (*plugin, bool) {
	pluginMu.RLock()
	defer pluginMu.RUnlock()
	p, ok := pluginDefs[t]
	return p, ok
}

type pluginRequest struct {
	Channel struct {
		ID      string            `json:"id"`
		Name    string            `json:"name"`
		Options map[string]string `json:"options"`
	} `json:"channel"`
	Subject string  `json:"subject"`
	Body    string  `json:"body"`
	Message Message `json:"message"`
}

type capBuffer struct {
	buf      bytes.Buffer
	max      int
	overflow bool
}

func (b *capBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		if len(p) <= room {
			_, _ = b.buf.Write(p)
		} else {
			_, _ = b.buf.Write(p[:room])
			b.overflow = true
		}
	} else {
		b.overflow = true
	}
	return len(p), nil
}

func runPlugin(p *plugin, ch ChannelConfig, subject, body string, m Message) error {
	var req pluginRequest
	req.Channel.ID = ch.ID
	req.Channel.Name = ch.Name
	req.Channel.Options = ch.Options
	req.Subject = subject
	req.Body = body
	req.Message = m
	payload, err := json.Marshal(&req)
	if err != nil {
		return fmt.Errorf("插件请求序列化失败: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.path, p.args...)
	cmd.Dir = p.dir
	cmd.Env = p.env
	// 插件可能再拉子进程；超时后不让孙进程持有管道把等待拖长。
	cmd.WaitDelay = time.Second
	cmd.Stdin = bytes.NewReader(payload)
	out := &capBuffer{max: pluginOutputLimit}
	cmd.Stdout = out
	cmd.Stderr = out

	err = cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("插件 %s 超时（%s）", p.def.Type, p.timeout)
	}
	if err != nil {
		if msg := sanitizePluginOutput(out.buf.String()); msg != "" {
			if out.overflow {
				msg += "…"
			}
			return fmt.Errorf("插件 %s 调用失败: %v: %s", p.def.Type, err, msg)
		}
		return fmt.Errorf("插件 %s 调用失败: %v", p.def.Type, err)
	}
	return nil
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// sanitizePluginOutput 压缩插件输出，避免把内部信息原样带进 API 响应。
// 去掉控制字符（含 ANSI 的 ESC 序列）与换行，只保留可读的单行摘要，
// 并截断到 160 个字符。
func sanitizePluginOutput(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f:
			// 丢弃控制字符
		default:
			b.WriteRune(r)
		}
	}
	return truncateRunes(strings.Join(strings.Fields(b.String()), " "), 160)
}

// pluginDefsSnapshot 返回按类型排序的插件定义副本。
func pluginDefsSnapshot() []ChannelDef {
	pluginMu.RLock()
	defer pluginMu.RUnlock()
	types := make([]string, 0, len(pluginDefs))
	for t := range pluginDefs {
		types = append(types, t)
	}
	sort.Strings(types)
	out := make([]ChannelDef, 0, len(types))
	for _, t := range types {
		d := pluginDefs[t].def
		d.Fields = append([]Field(nil), d.Fields...)
		out = append(out, d)
	}
	return out
}

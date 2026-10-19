// Package selfupdate 实现 Agent 的签名自更新。
//
// 设计约束：
//   - 只由 Agent 主动拉取清单，服务端不能下发更新指令
//   - 清单为 Ed25519 签名信封，下载资产必须匹配清单中的 SHA-256 与大小
//   - 下载地址默认必须 HTTPS；本机地址或显式 -allow-insecure 例外
//   - 更新包与当前可执行文件同目录落盘，通过 rename 原子替换
//
// 本包不 import os/exec，Agent「不执行命令」的承诺仍然成立。
package selfupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const (
	// MinAssetSize / MaxAssetSize 是更新包允许的大小范围。
	MinAssetSize    = 1 << 10
	MaxAssetSize    = 64 << 20
	maxEnvelopeSize = 256 << 10
	maxPayloadSize  = 128 << 10
)

var (
	assetKeyRe = regexp.MustCompile(`^[a-z0-9]+-[a-z0-9]+$`)
	sha256Re   = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
)

// Asset 描述单个平台的更新包。
type Asset struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Manifest 是验签后的清单。
type Manifest struct {
	Version     string           `json:"version"`
	PublishedAt int64            `json:"publishedAt"`
	Assets      map[string]Asset `json:"assets"`
}

type envelope struct {
	Schema    int    `json:"schema"`
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

type payload struct {
	Version     string           `json:"version"`
	PublishedAt int64            `json:"publishedAt"`
	Assets      map[string]Asset `json:"assets"`
}

// Updater 负责拉取清单与下载资产。
type Updater struct {
	ManifestURL   string
	PublicKey     ed25519.PublicKey
	AllowInsecure bool
	GOOS          string // 测试可覆盖；默认 runtime.GOOS
	GOARCH        string
	client        *http.Client
}

// New 校验清单地址与公钥。
func New(manifestURL, publicKeyB64 string, allowInsecure bool) (*Updater, error) {
	key, err := DecodePublicKey(publicKeyB64)
	if err != nil {
		return nil, err
	}
	manifestURL = strings.TrimSpace(manifestURL)
	if err := CheckURL(manifestURL, allowInsecure); err != nil {
		return nil, fmt.Errorf("update.manifestUrl: %w", err)
	}
	return &Updater{
		ManifestURL:   manifestURL,
		PublicKey:     key,
		AllowInsecure: allowInsecure,
		GOOS:          runtime.GOOS,
		GOARCH:        runtime.GOARCH,
		client: &http.Client{
			Timeout: 5 * time.Minute,
			// 不跟随重定向：避免清单/资产被引导到未校验的地址，甚至从 HTTPS 降级到 HTTP。
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// DecodePublicKey 解析 base64（标准编码）的 32 字节 Ed25519 公钥。
func DecodePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("update.publicKey 必须是 base64 编码的 32 字节 Ed25519 公钥")
	}
	return ed25519.PublicKey(b), nil
}

// CheckURL 校验更新地址：默认 HTTPS，本机或显式放行时允许 HTTP。
func CheckURL(raw string, allowInsecure bool) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	// 只检查 Host 非空会放过 https://:443：Host 是 ":443"，Hostname 为空。
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return errors.New("更新地址无效")
	}
	if u.User != nil {
		return errors.New("更新地址不能包含用户名或密码")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if allowInsecure || isLoopbackHost(u.Hostname()) {
			return nil
		}
		return errors.New("更新地址必须使用 HTTPS（或显式 -allow-insecure / 本机地址）")
	default:
		return errors.New("更新地址仅支持 http/https")
	}
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// VerifyEnvelope 验签并解析清单。签名覆盖的是 payload 的原始字节。
func VerifyEnvelope(raw []byte, pub ed25519.PublicKey) (*Manifest, error) {
	if len(raw) == 0 || len(raw) > maxEnvelopeSize {
		return nil, errors.New("更新清单大小异常")
	}
	var env envelope
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&env); err != nil {
		return nil, fmt.Errorf("更新清单格式错误: %w", err)
	}
	if env.Schema != 1 {
		return nil, fmt.Errorf("不支持的更新清单 schema: %d", env.Schema)
	}
	payloadRaw, err := base64.StdEncoding.DecodeString(env.Payload)
	if err != nil || len(payloadRaw) == 0 || len(payloadRaw) > maxPayloadSize {
		return nil, errors.New("更新清单 payload 无效")
	}
	sig, err := base64.StdEncoding.DecodeString(env.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, errors.New("更新清单签名格式无效")
	}
	// ed25519.Verify 对长度不合法的公钥会直接 panic（而不是返回 false）。
	// VerifyEnvelope 是导出入口，Updater.PublicKey 也是导出字段，
	// 调用方传入残缺配置时不应让进程崩溃。
	if len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("更新公钥长度无效")
	}
	if !ed25519.Verify(pub, payloadRaw, sig) {
		return nil, errors.New("更新清单签名校验失败")
	}

	var p payload
	pdec := json.NewDecoder(strings.NewReader(string(payloadRaw)))
	pdec.DisallowUnknownFields()
	if err := pdec.Decode(&p); err != nil {
		return nil, fmt.Errorf("更新清单内容无效: %w", err)
	}
	if _, ok := parseVersion(p.Version); !ok {
		return nil, fmt.Errorf("更新清单版本号无效: %q", p.Version)
	}
	if len(p.Assets) == 0 {
		return nil, errors.New("更新清单没有资产")
	}
	for k, a := range p.Assets {
		if !assetKeyRe.MatchString(k) {
			return nil, fmt.Errorf("更新清单资产键无效: %q", k)
		}
		if a.Size < MinAssetSize || a.Size > MaxAssetSize {
			return nil, fmt.Errorf("资产 %s 大小超出允许范围", k)
		}
		if !sha256Re.MatchString(a.SHA256) {
			return nil, fmt.Errorf("资产 %s 的 sha256 无效", k)
		}
		if a.URL == "" {
			return nil, fmt.Errorf("资产 %s 缺少 url", k)
		}
		// 验签阶段就拒绝非 http/https、带 userinfo 或空主机名的地址。
		// allowInsecure 传 true 只校验形状：是否允许明文 HTTP 是下载时的
		// 部署策略（Download 会再按 AllowInsecure 完整校验一次）。
		if err := CheckURL(a.URL, true); err != nil {
			return nil, fmt.Errorf("资产 %s 的 url 无效: %w", k, err)
		}
	}
	return &Manifest{Version: p.Version, PublishedAt: p.PublishedAt, Assets: p.Assets}, nil
}

// Fetch 拉取并验签清单。
func (u *Updater) Fetch(ctx context.Context) (*Manifest, error) {
	// Fetch 是导出方法，Updater 的字段也可写：不能假设调用方一定走 New。
	// 重新校验 URL（否则改成 http:// 就绕过 New 里的 HTTPS 要求，
	// 清单与签名可被中间人整体替换），并兜住零值 Updater 的 nil client。
	manifestURL := strings.TrimSpace(u.ManifestURL)
	if err := CheckURL(manifestURL, u.AllowInsecure); err != nil {
		return nil, fmt.Errorf("更新清单地址: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, manifestURL, nil)
	if err != nil {
		return nil, errors.New("构造更新清单请求失败")
	}
	resp, err := u.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("拉取更新清单失败: %w", netErr(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("拉取更新清单失败: HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxEnvelopeSize+1))
	if err != nil {
		return nil, fmt.Errorf("读取更新清单失败: %w", err)
	}
	if len(raw) > maxEnvelopeSize {
		return nil, errors.New("更新清单超过大小限制")
	}
	return VerifyEnvelope(raw, u.PublicKey)
}

// httpClient 返回底层客户端；零值 Updater 也能工作，而不是空指针崩溃。
// 未配置超时的客户端会无限等待，这里给一个保守默认值。
func (u *Updater) httpClient() *http.Client {
	if u.client != nil {
		return u.client
	}
	return &http.Client{
		Timeout: 30 * time.Second,
		// 与 New 保持一致：零值/手工构造的 Updater 也不能跟随重定向，
		// 否则清单或资产可能被引到 HTTP 或未校验的主机。
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Candidate 返回适用于当前平台的资产；第二个返回值表示是否有更新。
func (u *Updater) Candidate(m *Manifest, currentVersion string) (Asset, bool) {
	key := u.GOOS + "-" + u.GOARCH
	a, ok := m.Assets[key]
	if !ok {
		return Asset{}, false
	}
	return a, IsNewer(m.Version, currentVersion)
}

// ValidVersion 判断版本号是否可解析。
func ValidVersion(s string) bool {
	_, ok := parseVersion(s)
	return ok
}

// ValidAssetKey 判断平台键是否合法（如 linux-amd64）。
func ValidAssetKey(k string) bool { return assetKeyRe.MatchString(k) }

// SignManifest 生成签名信封，供 rollsign 工具与测试使用。
func SignManifest(m *Manifest, priv ed25519.PrivateKey) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, errors.New("Ed25519 私钥长度无效")
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	env := envelope{
		Schema:    1,
		Payload:   base64.StdEncoding.EncodeToString(raw),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, raw)),
	}
	return json.MarshalIndent(env, "", "  ")
}

// IsNewer 判断 remote 是否比 current 新；current 无法解析（如 dev）时允许更新。
func IsNewer(remote, current string) bool {
	rv, ok := parseVersion(remote)
	if !ok {
		return false
	}
	cv, ok := parseVersion(current)
	if !ok {
		return true
	}
	return rv.compare(cv) > 0
}

// SameVersion 判断两个版本字符串是否表示同一版本。
// 构建版本常带 v 前缀（v1.7.0），而清单中的版本可能不带（1.7.0），
// 启动标记与冷却比较必须按语义而非字符串相等。
func SameVersion(a, b string) bool {
	av, ok := parseVersion(a)
	bv, ok2 := parseVersion(b)
	return ok && ok2 && av.compare(bv) == 0
}

// canonicalVersion 统一存储格式：去掉 v 前缀与构建元数据，保留预发布后缀。
func canonicalVersion(s string) string {
	v, ok := parseVersion(s)
	if !ok {
		return strings.TrimSpace(s)
	}
	out := fmt.Sprintf("%d.%d.%d", v.n[0], v.n[1], v.n[2])
	if v.pre != "" {
		out += "-" + v.pre
	}
	return out
}

type version struct {
	n   [3]int
	pre string
}

func parseVersion(s string) (version, bool) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V"))
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	var v version
	if i := strings.IndexByte(s, '-'); i >= 0 {
		v.pre = s[i+1:]
		if v.pre == "" {
			// `1.2.3-`、`1.2.3-+build`：连字符后为空。
			// 接受它会让排序把「有预发布后缀」当成有效版本，语义混乱。
			return version{}, false
		}
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 1 || len(parts) > 3 {
		return version{}, false
	}
	for i := 0; i < 3; i++ {
		if i >= len(parts) {
			v.n[i] = 0
			continue
		}
		p := parts[i]
		if p == "" || len(p) > 6 {
			return version{}, false
		}
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				return version{}, false
			}
			n = n*10 + int(c-'0')
		}
		v.n[i] = n
	}
	if len([]rune(v.pre)) > 32 {
		return version{}, false
	}
	return v, true
}

func (a version) compare(b version) int {
	for i := 0; i < 3; i++ {
		if a.n[i] != b.n[i] {
			if a.n[i] > b.n[i] {
				return 1
			}
			return -1
		}
	}
	// 预发布版本低于正式版本
	if a.pre == b.pre {
		return 0
	}
	if a.pre == "" {
		return 1
	}
	if b.pre == "" {
		return -1
	}
	// 预发布标识按 semver 规则比较：数字段按数值、分段逐位比较。
	return comparePre(a.pre, b.pre)
}

// comparePre 比较两个预发布标识（a/b 均非空）。
// semver：以 . 分段逐位比较；数字段按数值，数字段低于非数字段；
// 前缀相同时段数少的更小（1.0.0-alpha < 1.0.0-alpha.1）。
func comparePre(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		xok, yok := isDigits(as[i]), isDigits(bs[i])
		switch {
		case xok && yok:
			if c := cmpNumeric(as[i], bs[i]); c != 0 {
				return c
			}
		case xok:
			return -1
		case yok:
			return 1
		default:
			if as[i] != bs[i] {
				if as[i] > bs[i] {
					return 1
				}
				return -1
			}
		}
	}
	switch {
	case len(as) < len(bs):
		return -1
	case len(as) > len(bs):
		return 1
	}
	return 0
}

// isDigits 判断是否为纯数字段（空串不算）。
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// cmpNumeric 比较两个纯数字段。
//
// 不能先转 int：32 位平台上 `n*10` 在 n 接近 1e9 时会回绕成负数，
// 上限截断还会让 1.0.0-1000000000 与 1.0.0-10000000000 变成「相等」，
// 于是 SameVersion/IsNewer 把不同版本判成同一个。
// 改用「去前导零后先比长度、再比字典序」：无溢出也不丢失精度。
func cmpNumeric(a, b string) int {
	ta := strings.TrimLeft(a, "0")
	tb := strings.TrimLeft(b, "0")
	if ta == "" {
		ta = "0"
	}
	if tb == "" {
		tb = "0"
	}
	switch {
	case len(ta) < len(tb):
		return -1
	case len(ta) > len(tb):
		return 1
	case ta < tb:
		return -1
	case ta > tb:
		return 1
	}
	return 0
}

// Download 下载资产到 dir（必须与可执行文件同目录），校验大小与 SHA-256。
// 返回的临时文件权限为 0600，由 Apply 调整为原可执行文件权限。
func (u *Updater) Download(ctx context.Context, a Asset, dir string) (string, error) {
	// 大小边界只在 VerifyEnvelope 里检查；Download 是导出方法，
	// 调用方可能传入自行构造的 Asset，这里必须再校验一次。
	if a.Size < MinAssetSize || a.Size > MaxAssetSize {
		return "", fmt.Errorf("更新资产大小 %d 超出允许范围", a.Size)
	}
	assetURL := strings.TrimSpace(a.URL)
	if err := CheckURL(assetURL, u.AllowInsecure); err != nil {
		return "", fmt.Errorf("更新资产地址: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return "", errors.New("构造更新下载请求失败")
	}
	resp, err := u.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("下载更新失败: %w", netErr(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载更新失败: HTTP %d", resp.StatusCode)
	}

	tmp, err := os.CreateTemp(dir, ".rollcall-agent-download-*")
	if err != nil {
		return "", fmt.Errorf("更新目录不可写: %w", err)
	}
	name := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(name)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, a.Size+1))
	if err != nil {
		cleanup()
		return "", fmt.Errorf("写入更新失败: %w", err)
	}
	if n != a.Size {
		cleanup()
		return "", fmt.Errorf("更新包大小不符: 收到 %d 字节，清单声明 %d", n, a.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, a.SHA256) {
		cleanup()
		return "", errors.New("更新包 SHA-256 校验失败")
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return "", fmt.Errorf("落盘更新包失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("关闭更新包失败: %w", err)
	}
	if err := os.Chmod(name, 0o600); err != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("设置更新包权限失败: %w", err)
	}
	return name, nil
}

// netErr 去掉 url.Error 中的完整地址，避免带查询串的清单地址进入日志。
func netErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}

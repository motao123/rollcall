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
	if err != nil || u.Host == "" {
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
	}
	return &Manifest{Version: p.Version, PublishedAt: p.PublishedAt, Assets: p.Assets}, nil
}

// Fetch 拉取并验签清单。
func (u *Updater) Fetch(ctx context.Context) (*Manifest, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.ManifestURL, nil)
	if err != nil {
		return nil, errors.New("构造更新清单请求失败")
	}
	resp, err := u.client.Do(req)
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
	if a.pre > b.pre {
		return 1
	}
	return -1
}

// Download 下载资产到 dir（必须与可执行文件同目录），校验大小与 SHA-256。
// 返回的临时文件权限为 0600，由 Apply 调整为原可执行文件权限。
func (u *Updater) Download(ctx context.Context, a Asset, dir string) (string, error) {
	if err := CheckURL(a.URL, u.AllowInsecure); err != nil {
		return "", fmt.Errorf("更新资产地址: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return "", errors.New("构造更新下载请求失败")
	}
	resp, err := u.client.Do(req)
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

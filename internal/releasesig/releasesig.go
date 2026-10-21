// Package releasesig 实现服务端发布二进制的离线 Ed25519 签名与验签。
//
// 与 Agent 自更新清单不同，这里签名的是单个二进制：签名同时绑定版本号、
// SHA-256 与文件大小。升级脚本在替换正在运行的二进制之前用受信任的公钥
// 验签，因此下载源被篡改也无法装上未签名的文件。
package releasesig

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/motao123/rollcall/internal/selfupdate"
)

const (
	// Schema 是签名文件格式版本；不兼容变更时必须递增。
	Schema = 1
	// MinArtifactSize / MaxArtifactSize 限定可签名的二进制大小。
	MinArtifactSize = 1 << 10
	MaxArtifactSize = 256 << 20
	// MaxSignatureSize 防止误把大文件当成签名读入内存。
	MaxSignatureSize = 8 << 10

	messagePrefix = "rollcall-release-v1\n"
)

var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Envelope 是发行签名的 JSON 文件格式。
type Envelope struct {
	Schema    int    `json:"schema"`
	Version   string `json:"version"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	Signature string `json:"signature"`
}

// Release 是验签通过的发布信息。
type Release struct {
	Version string
	SHA256  string
	Size    int64
}

// SignFile 计算文件的 SHA-256 与大小并生成签名信封。
func SignFile(path, version string, priv ed25519.PrivateKey) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, errors.New("Ed25519 私钥长度无效")
	}
	version = strings.TrimSpace(version)
	if !validVersion(version) {
		return nil, fmt.Errorf("版本号无效: %q", version)
	}
	sum, size, err := hashFile(path)
	if err != nil {
		return nil, err
	}
	env := Envelope{
		Schema:    Schema,
		Version:   version,
		SHA256:    sum,
		Size:      size,
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, signedMessage(version, sum, size))),
	}
	return json.MarshalIndent(env, "", "  ")
}

// Verify 验签并返回签名绑定的发布信息；不读取被签名的文件。
func Verify(raw []byte, pub ed25519.PublicKey) (*Release, error) {
	if len(raw) == 0 || len(raw) > MaxSignatureSize {
		return nil, errors.New("签名文件大小异常")
	}
	var env Envelope
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&env); err != nil {
		return nil, fmt.Errorf("签名文件格式错误: %w", err)
	}
	// 多个 JSON 值连写时 Decode 只读第一个；不检查会忽略夹带的内容。
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("签名文件包含多余内容")
	}
	if env.Schema != Schema {
		return nil, fmt.Errorf("不支持的签名 schema: %d", env.Schema)
	}
	if env.Version != strings.TrimSpace(env.Version) || !validVersion(env.Version) {
		return nil, fmt.Errorf("签名中的版本号无效: %q", env.Version)
	}
	if !sha256Re.MatchString(env.SHA256) {
		return nil, errors.New("签名中的 sha256 无效（应为 64 位小写 hex）")
	}
	if env.Size < MinArtifactSize || env.Size > MaxArtifactSize {
		return nil, fmt.Errorf("签名中的文件大小 %d 超出允许范围", env.Size)
	}
	sig, err := base64.StdEncoding.DecodeString(env.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, errors.New("签名格式无效")
	}
	// ed25519.Verify 对长度不合法的公钥会直接 panic。
	if len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("发行公钥长度无效")
	}
	if !ed25519.Verify(pub, signedMessage(env.Version, env.SHA256, env.Size), sig) {
		return nil, errors.New("发行签名校验失败")
	}
	return &Release{Version: env.Version, SHA256: env.SHA256, Size: env.Size}, nil
}

// VerifyFile 在 Verify 的基础上确认文件内容与签名一致。
func VerifyFile(path string, raw []byte, pub ed25519.PublicKey) (*Release, error) {
	rel, err := Verify(raw, pub)
	if err != nil {
		return nil, err
	}
	sum, size, err := hashFile(path)
	if err != nil {
		return nil, err
	}
	if size != rel.Size {
		return nil, fmt.Errorf("文件大小不符: 实际 %d 字节，签名声明 %d 字节", size, rel.Size)
	}
	if sum != rel.SHA256 {
		return nil, errors.New("文件 SHA-256 与发行签名不符")
	}
	return rel, nil
}

// DecodePublicKey 解析 base64（标准编码）的 32 字节 Ed25519 公钥。
func DecodePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("发行公钥必须是 base64 编码的 32 字节 Ed25519 公钥")
	}
	return ed25519.PublicKey(b), nil
}

func signedMessage(version, sha string, size int64) []byte {
	return []byte(fmt.Sprintf("%s%s\n%s\n%d\n", messagePrefix, version, sha, size))
}

// validVersion 复用 Agent 更新清单的版本规则，并额外拒绝换行与控制字符：
// 版本号会作为签名消息的一行，含换行会让消息边界产生歧义。
func validVersion(s string) bool {
	if s == "" || strings.ContainsAny(s, "\r\n\x00") {
		return false
	}
	return selfupdate.ValidVersion(s)
}

// hashFile 计算文件大小与 SHA-256。先按 Stat 大小做范围校验再读，
// 读取时再用 LimitReader 兜住 Stat 之后文件被追加的情况。
func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("读取文件失败: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", 0, fmt.Errorf("读取文件失败: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return "", 0, fmt.Errorf("不是普通文件: %s", path)
	}
	if fi.Size() < MinArtifactSize || fi.Size() > MaxArtifactSize {
		return "", 0, fmt.Errorf("文件大小 %d 超出允许范围", fi.Size())
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, MaxArtifactSize+1))
	if err != nil {
		return "", 0, fmt.Errorf("读取文件失败: %w", err)
	}
	if n < MinArtifactSize || n > MaxArtifactSize {
		return "", 0, fmt.Errorf("文件大小 %d 超出允许范围", n)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

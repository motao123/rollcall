package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultSessionTTL 是登录态有效期，同时由 Cookie Max-Age 约束。
	DefaultSessionTTL = 7 * 24 * time.Hour
	sessionKeySize    = 32
)

var sessionB64 = base64.RawURLEncoding

// Session 是验签后的会话内容。
type Session struct {
	Username string
	Version  int64
	Expires  time.Time
}

// Sessions 负责签发与验证 HMAC-SHA256 会话令牌。
type Sessions struct {
	key []byte
	ttl time.Duration
	now func() time.Time // 测试可注入
}

// NewSessions 创建会话管理器；key 至少 32 字节。
func NewSessions(key []byte, ttl time.Duration) (*Sessions, error) {
	if len(key) < sessionKeySize {
		return nil, errors.New("会话密钥至少需要 32 字节")
	}
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	k := make([]byte, len(key))
	copy(k, key)
	return &Sessions{key: k, ttl: ttl, now: time.Now}, nil
}

// TTL 返回会话有效期。
func (s *Sessions) TTL() time.Duration { return s.ttl }

// Issue 为指定用户签发会话。version 绑定当前密码版本，改密后旧会话失效。
func (s *Sessions) Issue(username string, version int64) (string, time.Time, error) {
	// 载荷用 '|' 分隔；用户名若含分隔符、换行或为空，Verify 会解析出错，
	// 或让不同用户名签出同一字段结构。这里复用登录名规则，只允许安全字符。
	if err := ValidateUsername(username); err != nil {
		return "", time.Time{}, err
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", time.Time{}, fmt.Errorf("生成会话随机数失败: %w", err)
	}
	exp := s.now().Add(s.ttl)
	payload := strings.Join([]string{
		"v1", username, strconv.FormatInt(version, 10),
		strconv.FormatInt(exp.Unix(), 10), sessionB64.EncodeToString(nonce),
	}, "|")
	raw := []byte(payload)
	sig := signSession(s.key, raw)
	token := sessionB64.EncodeToString(raw) + "." + sessionB64.EncodeToString(sig)
	return token, exp, nil
}

// Verify 校验签名、有效期与载荷格式。
func (s *Sessions) Verify(token string) (Session, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return Session{}, false
	}
	raw, err := sessionB64.DecodeString(parts[0])
	if err != nil {
		return Session{}, false
	}
	sig, err := sessionB64.DecodeString(parts[1])
	if err != nil {
		return Session{}, false
	}
	if subtle.ConstantTimeCompare(sig, signSession(s.key, raw)) != 1 {
		return Session{}, false
	}
	fields := strings.Split(string(raw), "|")
	if len(fields) != 5 || fields[0] != "v1" || fields[1] == "" {
		return Session{}, false
	}
	version, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return Session{}, false
	}
	expUnix, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil {
		return Session{}, false
	}
	if _, err := sessionB64.DecodeString(fields[4]); err != nil {
		return Session{}, false
	}
	exp := time.Unix(expUnix, 0)
	if !s.now().Before(exp) {
		return Session{}, false
	}
	return Session{Username: fields[1], Version: version, Expires: exp}, true
}

// TokenID 返回会话令牌的指纹，供服务端吊销列表持久化使用。
// 令牌本身是高熵随机值，明文摘要不会泄露签名密钥。
func (s *Sessions) TokenID(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func signSession(key, payload []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	return mac.Sum(nil)
}

// LoadOrCreateSessionKey 从文件读取会话签名密钥；不存在时生成。
// 密钥持久化后，服务重启不会让已登录用户掉线。
func LoadOrCreateSessionKey(path string) ([]byte, error) {
	key, err := readPrivate(path)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key = make([]byte, sessionKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("生成会话密钥失败: %w", err)
	}
	if err := createPrivate(path, key); err != nil {
		if errors.Is(err, os.ErrExist) {
			// 并发启动的另一个实例先创建了密钥：复用它，避免两边
			// 各签各的 Cookie，表现为用户随机掉线。
			return readPrivate(path)
		}
		return nil, err
	}
	return key, nil
}

// readPrivate 用 O_NOFOLLOW 打开并校验权限，读取最多 sessionKeySize 字节。
func readPrivate(path string) ([]byte, error) {
	f, err := openNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("会话密钥必须是普通文件")
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		if err := f.Chmod(0o600); err != nil {
			return nil, fmt.Errorf("会话密钥权限过宽且无法收紧: %w", err)
		}
	}
	raw, err := io.ReadAll(io.LimitReader(f, sessionKeySize+1))
	if err != nil {
		return nil, err
	}
	if len(raw) != sessionKeySize {
		return nil, fmt.Errorf("会话密钥长度无效: %d", len(raw))
	}
	return raw, nil
}

// createPrivate 仅在文件不存在时创建（O_EXCL）；已存在返回 os.ErrExist。
func createPrivate(path string, raw []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建密钥目录失败: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(path)
	}
	if err := f.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("设置会话密钥权限失败: %w", err)
	}
	if _, err := f.Write(raw); err != nil {
		cleanup()
		return fmt.Errorf("写入会话密钥失败: %w", err)
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("落盘会话密钥失败: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("关闭会话密钥失败: %w", err)
	}
	syncDir(dir)
	return nil
}

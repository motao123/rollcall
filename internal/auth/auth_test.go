package auth

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPBKDF2SHA256Vectors(t *testing.T) {
	// 公开的 PBKDF2-HMAC-SHA256 测试向量。
	cases := []struct {
		iter int
		want string
	}{
		{1, "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"},
		{2, "ae4d0c95af6b46d32d0adff928f06dd02a303f8ef3c251dfd6e2d85a95474c43"},
		{4096, "c5e478d59288c841aa530db6845c4c8d962893a001ce4e11a4963873aa98134a"},
	}
	for _, c := range cases {
		got := hex.EncodeToString(deriveKey([]byte("password"), []byte("salt"), c.iter, 32))
		if got != c.want {
			t.Fatalf("iter=%d 向量不匹配:\n got %s\nwant %s", c.iter, got, c.want)
		}
	}
}

func TestHashPassword(t *testing.T) {
	const password = "correct-horse-battery"
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "pbkdf2-sha256$600000$") {
		t.Fatalf("哈希格式异常: %s", hash)
	}
	if strings.Contains(hash, password) {
		t.Fatal("哈希中不应出现明文密码")
	}
	if err := ValidateHash(hash); err != nil {
		t.Fatalf("ValidateHash: %v", err)
	}
	if !VerifyPassword(hash, password) {
		t.Fatal("正确密码应通过校验")
	}
	if VerifyPassword(hash, "wrong-password-123") {
		t.Fatal("错误密码不应通过校验")
	}
	if VerifyPassword("garbage", password) {
		t.Fatal("非法哈希不应通过校验")
	}
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("过短密码应被拒绝")
	}
}

func TestUserCreateAuthenticateAndReset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admin-user.json")
	const pass = "initial-password-123"
	u, created, generated, err := LoadOrCreateUser(path, "admin", "Admin@Example.com", pass, false)
	if err != nil {
		t.Fatalf("创建管理员: %v", err)
	}
	if !created || generated != "" {
		t.Fatalf("created=%v generated=%q，期望首次创建且未生成随机密码", created, generated)
	}
	if u.Email != "admin@example.com" {
		t.Fatalf("邮箱应规范化为小写，得到 %q", u.Email)
	}
	if !u.Authenticate("admin", pass) || !u.Authenticate("ADMIN", pass) {
		t.Fatal("用户名应支持大小写不敏感登录")
	}
	if !u.Authenticate("admin@example.com", pass) {
		t.Fatal("邮箱应可登录")
	}
	if u.Authenticate("admin", "wrong-password-123") {
		t.Fatal("错误密码不应通过")
	}
	if u.Authenticate("nobody", pass) {
		t.Fatal("未知账号不应通过")
	}

	// 正常重启不会用新的环境变量密码覆盖已保存密码。
	if _, _, _, err := LoadOrCreateUser(path, "admin", "", "another-password-123", false); err != nil {
		t.Fatalf("重启加载: %v", err)
	}
	loaded, err := LoadUser(path)
	if err != nil {
		t.Fatalf("LoadUser: %v", err)
	}
	if !loaded.Authenticate("admin", pass) {
		t.Fatal("重启后旧密码应仍然有效")
	}

	// 重置密码后旧密码失效，会话版本递增。
	loaded.UpdatedAt = 1
	if err := saveUser(path, loaded); err != nil {
		t.Fatalf("saveUser: %v", err)
	}
	oldVersion := loaded.SessionVersion()
	u2, _, _, err := LoadOrCreateUser(path, "admin", "", "reset-password-456", true)
	if err != nil {
		t.Fatalf("重置密码: %v", err)
	}
	if u2.Authenticate("admin", pass) {
		t.Fatal("重置后旧密码不应再有效")
	}
	if !u2.Authenticate("admin", "reset-password-456") {
		t.Fatal("重置后新密码应有效")
	}
	if u2.SessionVersion() <= oldVersion {
		t.Fatal("改密后会话版本必须递增")
	}

	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		if _, err := LoadUser(path); err != nil {
			t.Fatalf("放宽权限后加载应自动收紧: %v", err)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Fatalf("权限未收紧: %v", fi.Mode().Perm())
		}
	}
}

func TestResetSessionVersionStrictlyIncreases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin-user.json")
	u, _, _, err := LoadOrCreateUser(path, "admin", "", "initial-password-123", false)
	if err != nil {
		t.Fatalf("创建管理员: %v", err)
	}
	u.UpdatedAt = time.Now().Unix()
	if err := saveUser(path, u); err != nil {
		t.Fatalf("saveUser: %v", err)
	}
	old := u.SessionVersion()
	u2, _, _, err := LoadOrCreateUser(path, "admin", "", "reset-password-456", true)
	if err != nil {
		t.Fatalf("重置密码: %v", err)
	}
	if u2.SessionVersion() <= old {
		t.Fatalf("同一秒内重置后会话版本必须递增: old=%d new=%d", old, u2.SessionVersion())
	}
}

func TestUserGeneratedPassword(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin-user.json")
	u, created, generated, err := LoadOrCreateUser(path, "admin", "", "", false)
	if err != nil {
		t.Fatalf("生成初始密码: %v", err)
	}
	if !created || len(generated) < MinPasswordLen {
		t.Fatalf("应生成至少 %d 位的随机密码，得到 %q", MinPasswordLen, generated)
	}
	if !u.Authenticate("admin", generated) {
		t.Fatal("生成的初始密码应可登录")
	}
	if strings.Contains(readFile(t, path), generated) {
		t.Fatal("配置文件中不应出现明文密码")
	}
}

func TestUserRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin-user.json")
	raw := `{"schema":1,"username":"admin","passwordHash":"x","updatedAt":1,"extra":true}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadUser(path); err == nil {
		t.Fatal("未知字段应被拒绝")
	}
}

func TestSessionIssueVerifyTamperAndExpiry(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	sm, err := NewSessions(key, time.Hour)
	if err != nil {
		t.Fatalf("NewSessions: %v", err)
	}
	fixed := time.Unix(1_800_000_000, 0)
	sm.now = func() time.Time { return fixed }

	token, exp, err := sm.Issue("admin", 42)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if !exp.Equal(fixed.Add(time.Hour)) {
		t.Fatalf("过期时间不符: %v", exp)
	}
	sess, ok := sm.Verify(token)
	if !ok || sess.Username != "admin" || sess.Version != 42 {
		t.Fatalf("Verify 失败: %+v ok=%v", sess, ok)
	}

	tampered := token[:len(token)-1] + string(token[len(token)-1]^1)
	if _, ok := sm.Verify(tampered); ok {
		t.Fatal("篡改后的令牌不应通过")
	}
	// base64 末位的未使用比特：换一种写法解码结果相同，但令牌原文不同。
	// 必须拒绝，否则按原文指纹记录的吊销列表会被绕过。
	parts := strings.Split(token, ".")
	sig, err := sessionB64.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("解码签名: %v", err)
	}
	var nonCanonical string
	for _, c := range "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_" {
		cand := parts[1][:len(parts[1])-1] + string(c)
		if cand == parts[1] {
			continue
		}
		dec, err := sessionB64.DecodeString(cand)
		if err == nil && string(dec) == string(sig) {
			nonCanonical = parts[0] + "." + cand
			break
		}
	}
	if nonCanonical == "" {
		t.Fatal("未构造出非规范 base64 写法")
	}
	if _, ok := sm.Verify(nonCanonical); ok {
		t.Fatal("非规范 base64 令牌不应通过")
	}
	other, _ := NewSessions([]byte(strings.Repeat("x", 32)), time.Hour)
	other.now = sm.now
	if _, ok := other.Verify(token); ok {
		t.Fatal("不同密钥签发的令牌不应通过")
	}

	sm.now = func() time.Time { return exp }
	if _, ok := sm.Verify(token); ok {
		t.Fatal("过期令牌不应通过")
	}
}

func TestSessionKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.key")
	key, err := LoadOrCreateSessionKey(path)
	if err != nil {
		t.Fatalf("创建密钥: %v", err)
	}
	if len(key) != sessionKeySize {
		t.Fatalf("密钥长度 %d", len(key))
	}
	again, err := LoadOrCreateSessionKey(path)
	if err != nil {
		t.Fatalf("读取密钥: %v", err)
	}
	if string(key) != string(again) {
		t.Fatal("重启后密钥应保持一致")
	}
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateSessionKey(path); err == nil {
		t.Fatal("长度错误的密钥文件应被拒绝")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

package auth

import (
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
	"time"
)

// RFC 6238 附录 B 的 SHA-1 测试向量（原始为 8 位码，这里取后 6 位）。
func TestTOTP_RFC6238Vectors(t *testing.T) {
	const secret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" // base32("12345678901234567890")
	cases := []struct {
		unix int64
		want string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
		{20000000000, "353130"},
	}
	for _, c := range cases {
		got, err := totpAt(secret, time.Unix(c.unix, 0))
		if err != nil {
			t.Fatalf("t=%d 计算失败: %v", c.unix, err)
		}
		if got != c.want {
			t.Errorf("t=%d: got %s, want %s", c.unix, got, c.want)
		}
	}
}

func TestVerifyTOTPWindow(t *testing.T) {
	const secret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	now := time.Unix(1_234_567_890, 0)
	code, err := totpAt(secret, now)
	if err != nil {
		t.Fatal(err)
	}
	if code != "005924" {
		t.Fatalf("前置条件失败: code=%s", code)
	}
	// 允许前后各一个时间窗的时钟偏差。
	for _, at := range []time.Time{now, now.Add(-30 * time.Second), now.Add(30 * time.Second)} {
		if !VerifyTOTP(secret, code, at) {
			t.Errorf("%s 应接受窗口内的动态码", at)
		}
	}
	for _, at := range []time.Time{now.Add(-60 * time.Second), now.Add(60 * time.Second)} {
		if VerifyTOTP(secret, code, at) {
			t.Errorf("%s 不应接受窗口外的动态码", at)
		}
	}
	// 密钥允许小写与空格，方便用户从验证器复制。
	if !VerifyTOTP("gezd gnbv gy3t qojq gezd gnbv gy3t qojq", code, now) {
		t.Error("带空格/小写的密钥应被规范化后校验")
	}
}

func TestVerifyTOTPRejectsMalformed(t *testing.T) {
	const secret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	now := time.Unix(1_234_567_890, 0)
	for _, code := range []string{"", "12345", "1234567", "12345a", "12345 ", "-12345"} {
		if VerifyTOTP(secret, code, now) {
			t.Errorf("畸形动态码 %q 不应通过", code)
		}
	}
	if VerifyTOTP("not-base32!!!", "123456", now) {
		t.Error("非法密钥不应通过")
	}
}

// TestUpdateTOTPKeepsPendingSecret 回归：仅保存待确认密钥时不能递增会话版本，
// 否则扫码后下一次页面请求就会把当前会话登出，启用流程无法完成。
func TestUpdateTOTPKeepsPendingSecret(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/admin-user.json"
	hash := "pbkdf2-sha256$100000$" +
		base64.RawStdEncoding.EncodeToString(make([]byte, 16)) + "$" +
		base64.RawStdEncoding.EncodeToString(make([]byte, 32))
	cur := &User{Schema: userSchema, Username: "admin", PasswordHash: hash, UpdatedAt: 1000}

	secret, err := NewTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	// 先保存待确认密钥（enabled=false）：版本不变。
	next, err := UpdateTOTP(path, cur, secret, false)
	if err != nil {
		t.Fatalf("保存待确认密钥失败: %v", err)
	}
	if next.TOTPSecret != secret || next.TOTPEnabled {
		t.Fatalf("待确认密钥未保存: %+v", next)
	}
	if next.UpdatedAt != cur.UpdatedAt {
		t.Errorf("待确认密钥不应改变会话版本: %d -> %d", cur.UpdatedAt, next.UpdatedAt)
	}

	// 启用后必须递增版本，让其他会话立即失效。
	enabled, err := UpdateTOTP(path, next, secret, true)
	if err != nil {
		t.Fatalf("启用 TOTP 失败: %v", err)
	}
	if !enabled.TOTPEnabled || enabled.UpdatedAt <= next.UpdatedAt {
		t.Errorf("启用后应递增版本: %+v", enabled)
	}

	// 非法密钥不能启用。
	if _, err := UpdateTOTP(path, enabled, "not-base32", true); err == nil {
		t.Error("非法密钥应被拒绝")
	}
}

func TestTOTPURL(t *testing.T) {
	raw := TOTPURL("Rollcall", "admin", "ABC234")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("otpauth URL 无法解析: %v", err)
	}
	if u.Scheme != "otpauth" || u.Host != "totp" {
		t.Errorf("URL 形态错误: %s", raw)
	}
	if !strings.Contains(u.Path, "Rollcall") || !strings.Contains(u.Path, "admin") {
		t.Errorf("label 缺少 issuer/account: %s", u.Path)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"secret": "ABC234", "issuer": "Rollcall", "algorithm": "SHA1", "digits": "6", "period": "30",
	} {
		if got := q.Get(k); got != want {
			t.Errorf("参数 %s=%q, 期望 %q", k, got, want)
		}
	}
}

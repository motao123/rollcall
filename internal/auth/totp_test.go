package auth

import (
	"encoding/base64"
	"errors"
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

// TestVerifyTOTPStepReturnsWindow 确认返回的是命中的时间步，
// 且前后一个窗口的码会映射到各自真实的步进（防重放依赖它做单调比较）。
func TestVerifyTOTPStepReturnsWindow(t *testing.T) {
	secret, err := NewTOTPSecret()
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	now := time.Unix(1790000000, 0)
	curStep := now.Unix() / 30

	for _, offset := range []int64{-1, 0, 1} {
		at := now.Add(time.Duration(offset) * 30 * time.Second)
		code, err := TOTPCode(secret, at)
		if err != nil {
			t.Fatalf("计算动态码失败: %v", err)
		}
		got, ok := VerifyTOTPStep(secret, code, now)
		if !ok {
			t.Fatalf("时间步 %d 的码应被接受", curStep+offset)
		}
		if got != curStep+offset {
			t.Errorf("时间步 = %d，期望 %d", got, curStep+offset)
		}
	}
}

// TestMarkTOTPStepRejectsReplay 是防重放的核心回归：
// 同一个时间步第二次消费必须失败，更早的步进也必须失败。
func TestMarkTOTPStepRejectsReplay(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/admin-user.json"
	u, _, _, err := LoadOrCreateUser(path, "admin", "", "Passw0rd-Long!", false)
	if err != nil {
		t.Fatalf("创建账号失败: %v", err)
	}
	step := time.Now().Unix() / 30

	first, err := MarkTOTPStep(path, u, step)
	if err != nil {
		t.Fatalf("首次消费应成功: %v", err)
	}
	if first.LastTOTPStep != step {
		t.Fatalf("已用步进未落盘: %d", first.LastTOTPStep)
	}
	if _, err := MarkTOTPStep(path, first, step); !errors.Is(err, ErrTOTPReplayed) {
		t.Fatalf("同一步进重放应被拒绝，实际 %v", err)
	}
	if _, err := MarkTOTPStep(path, first, step-1); !errors.Is(err, ErrTOTPReplayed) {
		t.Fatalf("更早的步进也应被拒绝（防止用旧窗口绕过），实际 %v", err)
	}
	// 落盘后重新加载，已用步进仍在（重启不能重置防重放状态）
	reloaded, _, _, err := LoadOrCreateUser(path, "", "", "", false)
	if err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	if reloaded.LastTOTPStep != step {
		t.Errorf("重启后已用步进丢失: %d", reloaded.LastTOTPStep)
	}
}

// TestTOTPSecretMinLength 回归：密钥下限必须是 128 位。
// 旧实现（10 字节 = 80 位）会静默接受手工写入的弱密钥。
func TestTOTPSecretMinLength(t *testing.T) {
	short := totpEnc.EncodeToString(make([]byte, 10))
	if _, err := decodeTOTPSecret(short); err == nil {
		t.Fatal("80 位密钥应被拒绝（RFC 4226 要求 ≥128 位）")
	}
	ok := totpEnc.EncodeToString(make([]byte, 16))
	if _, err := decodeTOTPSecret(ok); err != nil {
		t.Fatalf("128 位密钥应被接受: %v", err)
	}
}

// TestUpdateTOTPRotatesSecretBumpsVersion 回归：
// 已启用状态下轮换密钥（enabled 不变）也必须递增会话版本，
// 否则用旧密钥派生的动态码仍能登录，等于轮换没生效。
func TestUpdateTOTPRotatesSecretBumpsVersion(t *testing.T) {
	path := t.TempDir() + "/admin-user.json"
	u, _, _, err := LoadOrCreateUser(path, "admin", "", "Passw0rd-Long!", false)
	if err != nil {
		t.Fatalf("创建账号失败: %v", err)
	}
	secretA, err := NewTOTPSecret()
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	enabled, err := UpdateTOTP(path, u, secretA, true)
	if err != nil {
		t.Fatalf("启用失败: %v", err)
	}
	base := enabled.SessionVersion()

	// 同密钥重复保存：不应改变版本（幂等）
	same, err := UpdateTOTP(path, enabled, secretA, true)
	if err != nil {
		t.Fatalf("重复保存失败: %v", err)
	}
	if same.SessionVersion() != base {
		t.Errorf("密钥未变时会话版本不应变化: %d -> %d", base, same.SessionVersion())
	}

	// 轮换密钥：版本必须递增，让旧密钥签发的会话与动态码失去意义
	secretB, err := NewTOTPSecret()
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	rotated, err := UpdateTOTP(path, same, secretB, true)
	if err != nil {
		t.Fatalf("轮换失败: %v", err)
	}
	if rotated.SessionVersion() <= base {
		t.Errorf("轮换密钥后会话版本必须递增: %d -> %d", base, rotated.SessionVersion())
	}
}

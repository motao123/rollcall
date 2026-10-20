package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/auth"
)

// enableTOTP 在测试环境里把两步验证打开，返回密钥与换发后的 Cookie。
// 启用会递增会话版本，旧 Cookie 失效，必须用响应里换发的新会话继续。
func enableTOTP(t *testing.T, h http.Handler, ip string) (secret string, cookie *http.Cookie) {
	t.Helper()
	w := loginReq(h, ip, "imotao", testAdminPassword)
	if w.Code != http.StatusOK {
		t.Fatalf("登录失败: %d %s", w.Code, w.Body.String())
	}
	cookie = findSessionCookie(t, w)
	hdr := map[string]string{"Content-Type": "application/json", "X-Requested-With": "XMLHttpRequest"}

	w = doAuthReq(h, http.MethodPost, "/api/admin/account/totp", ip,
		`{"action":"setup","currentPassword":`+mustJSON(testAdminPassword)+`}`, hdr, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("生成 TOTP 密钥失败: %d %s", w.Code, w.Body.String())
	}
	var setup struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &setup); err != nil || setup.Secret == "" {
		t.Fatalf("响应缺少 secret: %s", w.Body.String())
	}
	code, err := auth.TOTPCode(setup.Secret, time.Now())
	if err != nil {
		t.Fatalf("计算动态码失败: %v", err)
	}
	w = doAuthReq(h, http.MethodPost, "/api/admin/account/totp", ip,
		`{"action":"enable","currentPassword":`+mustJSON(testAdminPassword)+`,"code":"`+code+`"}`, hdr, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("启用 TOTP 失败: %d %s", w.Code, w.Body.String())
	}
	return setup.Secret, findSessionCookie(t, w)
}

// TestTOTPReplayRejectedOnLogin 是 P2 回归（防重放）：
//
// 修复前 VerifyTOTP 只做无状态校验，同一个 6 位码在 ±1 窗口（约 90 秒）内
// 可以重复登录。捕获一次码（肩窥 / 钓鱼代理 / 明文 HTTP 中间人）就能再用一次。
// 修复后每个时间步只能被消费一次，且已用步进随账号落盘（重启不清零）。
func TestTOTPReplayRejectedOnLogin(t *testing.T) {
	h, _ := newAuthWeb(t, time.Hour)
	const ip = "10.30.0.1"
	secret, _ := enableTOTP(t, h, ip)

	code, err := auth.TOTPCode(secret, time.Now())
	if err != nil {
		t.Fatalf("计算动态码失败: %v", err)
	}
	body := `{"identifier":"imotao","password":` + mustJSON(testAdminPassword) + `,"totp":"` + code + `"}`

	if w := doAuthReq(h, http.MethodPost, "/api/auth/login", ip, body,
		map[string]string{"Content-Type": "application/json"}, nil); w.Code != http.StatusOK {
		t.Fatalf("首次用动态码登录应成功，实际 %d: %s", w.Code, w.Body.String())
	}
	// 同一个码再登录一次：必须被拒绝（重放）
	w := doAuthReq(h, http.MethodPost, "/api/auth/login", ip, body,
		map[string]string{"Content-Type": "application/json"}, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("重放同一个动态码应 401，实际 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "已使用") {
		t.Errorf("重放响应应说明动态码已使用，实际: %s", w.Body.String())
	}
}

// TestSensitiveAccountChangeRequiresTOTP 是 P2 回归（二次验证闭环）：
//
// 修复前改用户名/邮箱/密码只验当前密码，被窃会话 + 密码即可完成账号接管，
// 2FA 在最需要它的场景完全不起作用。修复后启用 TOTP 时这些操作必须带动态码，
// 且动态码同样一次性（重放返回 403）。
func TestSensitiveAccountChangeRequiresTOTP(t *testing.T) {
	h, _ := newAuthWeb(t, time.Hour)
	const ip = "10.31.0.1"
	secret, cookie := enableTOTP(t, h, ip)
	hdr := map[string]string{"Content-Type": "application/json", "X-Requested-With": "XMLHttpRequest"}

	// 1) 不带动态码改密码 → 400（提示需要动态码，且不计入限速）
	body := `{"currentPassword":` + mustJSON(testAdminPassword) + `,"newPassword":"new-passphrase-2026"}`
	w := doAuthReq(h, http.MethodPost, "/api/admin/account/password", ip, body, hdr, cookie)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("启用 2FA 后改密码不带动态码应 400，实际 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "动态验证码") {
		t.Errorf("响应应提示需要动态验证码，实际: %s", w.Body.String())
	}

	// 2) 带正确动态码 → 200，并换发新 Cookie（版本递增）
	code, err := auth.TOTPCode(secret, time.Now())
	if err != nil {
		t.Fatalf("计算动态码失败: %v", err)
	}
	body = `{"currentPassword":` + mustJSON(testAdminPassword) + `,"newPassword":"new-passphrase-2026","code":"` + code + `"}`
	w = doAuthReq(h, http.MethodPost, "/api/admin/account/password", ip, body, hdr, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("带正确动态码改密码应成功，实际 %d: %s", w.Code, w.Body.String())
	}
	cookie = findSessionCookie(t, w)

	// 3) 同一个动态码再改一次 → 403（重放）
	body = `{"currentPassword":"new-passphrase-2026","newPassword":"another-passphrase-2026","code":"` + code + `"}`
	w = doAuthReq(h, http.MethodPost, "/api/admin/account/password", ip, body, hdr, cookie)
	if w.Code != http.StatusForbidden {
		t.Fatalf("重放动态码应 403，实际 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "已使用") {
		t.Errorf("重放响应应说明动态码已使用，实际: %s", w.Body.String())
	}
}

package web

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/auth"
)

// TestLoginClearsBlockedReadonlyScope 是 P1 回归：
//
// 中间件把**任何** 401 都记为失败，而"会话过期"也返回 401。
// SPA 首屏会并行拉 2–5 个受保护接口，会话一过期就会在 IP|readonly 上
// 累积 10 次失败 → 该 IP 被封 10 分钟。如果登录成功只清 authLogin 作用域，
// 用户重新登录后仍然处处 429，且没有自愈路径（表现为"登录成功但打不开"）。
//
// 修复前基线：本用例在最后一步会拿到 429。
func TestLoginClearsBlockedReadonlyScope(t *testing.T) {
	h, _ := newAuthWeb(t, time.Hour)
	const ip = "10.20.0.1"

	// 模拟会话过期：对受保护接口连打 10 次无凭据请求
	for i := 0; i < 10; i++ {
		if w := doAuthReq(h, http.MethodGet, "/api/nodes", ip, "", nil, nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次无凭据请求应 401，实际 %d", i+1, w.Code)
		}
	}
	// 确认该作用域确实已被封（否则本测试的前提不成立）
	if w := doAuthReq(h, http.MethodGet, "/api/nodes", ip, "", nil, nil); w.Code != http.StatusTooManyRequests {
		t.Fatalf("连续 401 后该来源应被限速，实际 %d", w.Code)
	}

	// 用正确密码登录：登录端点自身在 authPublic 作用域，不受 readonly 封禁影响
	w := loginReq(h, ip, "demo-admin", testAdminPassword)
	if w.Code != http.StatusOK {
		t.Fatalf("登录应成功，实际 %d: %s", w.Code, w.Body.String())
	}
	cookie := findSessionCookie(t, w)

	// 关键断言：登录后受保护接口必须立即可用
	if got := doAuthReq(h, http.MethodGet, "/api/nodes", ip, "", nil, cookie); got.Code != http.StatusOK {
		t.Fatalf("登录成功后受保护接口应 200（登录必须解除该 IP 的所有作用域封禁），实际 %d: %s",
			got.Code, got.Body.String())
	}
}

// TestTOTPFailureCountsTowardLimit 是 P1 回归：
//
// 停用/启用两步验证时动态码错误曾返回 400，而 accountChange 只对 403 计失败，
// 400 落在 default: release 分支 → 失败计数永不增长。
// 拿到会话 + 当前密码的攻击者可以无速率限制地穷举 6 位动态码把 2FA 关掉。
//
// 修复前基线：第 11 次错误动态码仍返回 403（而非 429）。
func TestTOTPFailureCountsTowardLimit(t *testing.T) {
	h, _ := newAuthWeb(t, time.Hour)
	const ip = "10.21.0.1"

	w := loginReq(h, ip, "demo-admin", testAdminPassword)
	if w.Code != http.StatusOK {
		t.Fatalf("登录失败: %d %s", w.Code, w.Body.String())
	}
	cookie := findSessionCookie(t, w)
	csrf := map[string]string{"Content-Type": "application/json", "X-Requested-With": "XMLHttpRequest"}

	// 先绑定密钥，再用正确动态码启用，才能走到"停用需要动态码"的分支。
	w = doAuthReq(h, http.MethodPost, "/api/admin/account/totp", ip,
		`{"action":"setup","currentPassword":`+mustJSON(testAdminPassword)+`}`, csrf, cookie)
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
		`{"action":"enable","currentPassword":`+mustJSON(testAdminPassword)+`,"code":"`+code+`"}`, csrf, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("启用 TOTP 失败: %d %s", w.Code, w.Body.String())
	}
	// 启用 2FA 会递增会话版本，旧 Cookie 立即失效；
	// 服务端在响应里换发了新会话，必须用它继续后面的请求。
	cookie = findSessionCookie(t, w)

	// 连续用错误动态码尝试停用：应逐步累积失败，最终被限速。
	// 注意动态码每次都要变（同一窗口内相同值可能被重放，不在这里验证）。
	for i := 0; i < 10; i++ {
		body := `{"action":"disable","currentPassword":` + mustJSON(testAdminPassword) + `,"code":"000000"}`
		got := doAuthReq(h, http.MethodPost, "/api/admin/account/totp", ip, body, csrf, cookie)
		if got.Code != http.StatusForbidden {
			t.Fatalf("第 %d 次错误动态码应 403（计入失败），实际 %d: %s", i+1, got.Code, got.Body.String())
		}
	}
	body := `{"action":"disable","currentPassword":` + mustJSON(testAdminPassword) + `,"code":"000000"}`
	if got := doAuthReq(h, http.MethodPost, "/api/admin/account/totp", ip, body, csrf, cookie); got.Code != http.StatusTooManyRequests {
		t.Fatalf("连续错误动态码后应限速（429），实际 %d——说明 TOTP 失败没有计入限速", got.Code)
	}
}

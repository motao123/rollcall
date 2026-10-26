package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/motao123/rollcall/internal/store"
)

// createToken 用管理令牌建一个作用域令牌，返回明文。
func createToken(t *testing.T, h http.Handler, adminTok, body string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/tokens", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin-Token", adminTok)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("创建令牌应 201，实际 %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析创建响应: %v", err)
	}
	if !strings.HasPrefix(out.Token, APITokenPrefix) {
		t.Fatalf("返回的令牌应以 %s 开头，实际 %q", APITokenPrefix, out.Token)
	}
	return out.Token
}

// auditFor 数指定动作的审计条目。
func auditFor(st *store.Store, action string) int {
	entries, total := st.QueryAudit(store.AuditQuery{Limit: 200, Action: action})
	if total < len(entries) {
		return total
	}
	return len(entries)
}

// TestTokenReadIsAudited 是 v1.34.0 回归：令牌（含 rollcall-mcp）读了什么，
// 过去完全不可见 —— 审计只记写操作。而「MCP 被用来查了什么、从哪台机器查的」
// 正是泄露或误授权时最需要回答的问题。
func TestTokenReadIsAudited(t *testing.T) {
	const admin = "token-audit-admin-1234567890"
	st := publicStore(t)
	srv := New(st, nil, nil, Config{AdminToken: admin})
	h := srv.Handler()

	// 不限路径：本用例要验证"不同路径各记一条"。
	// 路径限制本身由 tokens_scope 的用例覆盖。
	raw := createToken(t, h, admin, `{"name":"mcp-ci","scopes":["read"]}`)

	get := func(path, bearer string, remote string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		req.RemoteAddr = remote
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	if w := get("/api/overview", raw, "203.0.113.7:5555"); w.Code != http.StatusOK {
		t.Fatalf("令牌读应 200，实际 %d: %s", w.Code, w.Body.String())
	}

	entries, _ := st.QueryAudit(store.AuditQuery{Limit: 50, Action: "api.read"})
	if len(entries) != 1 {
		t.Fatalf("应有 1 条 api.read 审计，实际 %d 条", len(entries))
	}
	e := entries[0]
	if e.Actor != "mcp-ci" || e.Role != "api-token" {
		t.Errorf("审计主体应为令牌名与 api-token 角色，实际 actor=%q role=%q", e.Actor, e.Role)
	}
	if e.IP != "203.0.113.7" {
		t.Errorf("应记录令牌的来源地址，实际 %q", e.IP)
	}
	if e.Path != "/api/overview" || e.Status != http.StatusOK {
		t.Errorf("应记录路径与状态，实际 path=%q status=%d", e.Path, e.Status)
	}

	// 同一令牌在同一路径上的重复读合并，否则 MCP 一次会话就能刷出几十条。
	for i := 0; i < 5; i++ {
		get("/api/overview", raw, "203.0.113.7:5555")
	}
	if n := auditFor(st, "api.read"); n != 1 {
		t.Errorf("窗口内重复读应合并为 1 条，实际 %d 条", n)
	}

	// 换一条路径要单独记：回答"读过哪些数据"靠的就是这个。
	get("/api/nodes", raw, "203.0.113.7:5555")
	if n := auditFor(st, "api.read"); n != 2 {
		t.Errorf("不同路径应各记一条，实际 %d 条", n)
	}
}

// TestTokenReadAuditSkipsNonToken 确认审计不记录浏览器会话与匿名请求，
// 否则面板每几秒的轮询会把审计文件刷满，反而淹没真正的线索。
func TestTokenReadAuditSkipsNonToken(t *testing.T) {
	const admin = "token-audit-admin-2234567890"
	st := publicStore(t)
	srv := New(st, nil, nil, Config{AdminToken: admin})
	h := srv.Handler()

	for _, path := range []string{"/api/status", "/healthz"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "198.51.100.9:1234"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
	}
	if n := auditFor(st, "api.read"); n != 0 {
		t.Errorf("无令牌的读请求不应产生 api.read 审计，实际 %d 条", n)
	}
}

// TestTokenLastUsedIP 确认令牌台账记录来源地址。
func TestTokenLastUsedIP(t *testing.T) {
	const admin = "token-audit-admin-3234567890"
	st := publicStore(t)
	srv := New(st, nil, nil, Config{AdminToken: admin})
	h := srv.Handler()

	raw := createToken(t, h, admin, `{"name":"ip-test","scopes":["read"]}`)
	req := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	req.RemoteAddr = "192.0.2.44:9999"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("读应 200，实际 %d", w.Code)
	}

	var got string
	for _, tk := range st.APITokens() {
		if tk.Name == "ip-test" {
			got = tk.LastUsedIP
		}
	}
	if got != "192.0.2.44" {
		t.Fatalf("令牌应记录来源地址 192.0.2.44，实际 %q", got)
	}
}

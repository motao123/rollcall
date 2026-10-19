package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/motao/rollcall/internal/auth"
	"github.com/motao/rollcall/internal/store"
)

const testAdminPassword = "correct-horse-battery-staple"

func newAuthWeb(t *testing.T, ttl time.Duration) (http.Handler, *auth.User) {
	t.Helper()
	dir := t.TempDir()
	u, created, generated, err := auth.LoadOrCreateUser(
		filepath.Join(dir, "admin-user.json"),
		"imotao", "Login@Example.com", testAdminPassword, false)
	if err != nil {
		t.Fatalf("创建测试管理员失败: %v", err)
	}
	if !created || generated != "" {
		t.Fatalf("测试应使用显式密码创建账号: created=%v generated=%q", created, generated)
	}
	sessions, err := auth.NewSessions(bytes.Repeat([]byte{0x42}, 32), ttl)
	if err != nil {
		t.Fatalf("创建会话管理器失败: %v", err)
	}
	srv := New(store.New(dir), nil, nil, Config{
		AdminToken: "web-test-admin-token-123456",
		User:       u,
		Sessions:   sessions,
	})
	return srv.Handler(), u
}

func doAuthReq(h http.Handler, method, path, ip, body string, headers map[string]string, cookie *http.Cookie) *httptest.ResponseRecorder {
	var r io.Reader
	if body != "" {
		r = bytes.NewBufferString(body)
	}
	req := httptest.NewRequest(method, path, r)
	req.RemoteAddr = ip + ":1234"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func loginReq(h http.Handler, ip, identifier, password string) *httptest.ResponseRecorder {
	return doAuthReq(h, http.MethodPost, "/api/auth/login", ip,
		`{"identifier":`+mustJSON(identifier)+`,"password":`+mustJSON(password)+`}`,
		map[string]string{"Content-Type": "application/json"}, nil)
}

func mustJSON(v string) string {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func findSessionCookie(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	t.Fatalf("响应缺少 %s Cookie: %v", sessionCookieName, w.Header().Values("Set-Cookie"))
	return nil
}

func decodeSession(t *testing.T, w *httptest.ResponseRecorder) (bool, map[string]string) {
	t.Helper()
	var got struct {
		Authenticated bool              `json:"authenticated"`
		User          map[string]string `json:"user"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析会话响应失败: %v (%s)", err, w.Body.String())
	}
	return got.Authenticated, got.User
}

// TestAuthLoginSessionFlow 覆盖账号登录、Cookie 属性、会话鉴权、
// CSRF 校验与退出登录的完整链路。
func TestAuthLoginSessionFlow(t *testing.T) {
	h, _ := newAuthWeb(t, time.Hour)

	w := doAuthReq(h, http.MethodGet, "/api/auth/session", "10.10.0.1", "", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("未登录探测会话应 200，实际 %d", w.Code)
	}
	if authed, _ := decodeSession(t, w); authed {
		t.Fatal("未登录会话不应返回 authenticated=true")
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("会话响应应禁止缓存，实际 Cache-Control=%q", cc)
	}

	w = loginReq(h, "10.10.0.2", "imotao", "wrong-password-123")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("错误密码应 401，实际 %d", w.Code)
	}

	w = doAuthReq(h, http.MethodPost, "/api/auth/login", "10.10.0.3",
		`{"identifier":"imotao","password":"`+testAdminPassword+`","extra":1}`,
		map[string]string{"Content-Type": "application/json"}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("未知字段应 400，实际 %d", w.Code)
	}

	// 带 X-Forwarded-Proto: https 登录，Cookie 必须标记 Secure。
	w = doAuthReq(h, http.MethodPost, "/api/auth/login", "10.10.0.4",
		`{"identifier":"imotao","password":"`+testAdminPassword+`"}`,
		map[string]string{"Content-Type": "application/json", "X-Forwarded-Proto": "https"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("正确密码应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	cookie := findSessionCookie(t, w)
	if !cookie.HttpOnly {
		t.Error("会话 Cookie 必须是 HttpOnly")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("会话 Cookie 必须是 SameSite=Strict，实际 %v", cookie.SameSite)
	}
	if !cookie.Secure {
		t.Error("HTTPS 请求下会话 Cookie 必须标记 Secure")
	}
	authed, user := decodeSession(t, w)
	if !authed || user["username"] != "imotao" || user["email"] != "login@example.com" {
		t.Fatalf("登录响应用户信息不符: %v", user)
	}

	w = doAuthReq(h, http.MethodGet, "/api/auth/session", "10.10.0.5", "", nil, cookie)
	if authed, _ := decodeSession(t, w); !authed {
		t.Fatal("携带有效 Cookie 应返回已登录")
	}

	if w = doAuthReq(h, http.MethodGet, "/api/admin/nodes", "10.10.0.6", "", nil, cookie); w.Code != http.StatusOK {
		t.Fatalf("会话应能读取只读接口，实际 %d", w.Code)
	}

	// 会话写操作必须带 X-Requested-With；缺失时 403（不是 401，避免误计爆破）。
	if w = doAuthReq(h, http.MethodPost, "/api/admin/nodes", "10.10.0.7",
		`{"name":"node-a"}`, map[string]string{"Content-Type": "application/json"}, cookie); w.Code != http.StatusForbidden {
		t.Fatalf("会话写操作缺少同源头应 403，实际 %d", w.Code)
	}
	if w = doAuthReq(h, http.MethodPost, "/api/admin/nodes", "10.10.0.8",
		`{"name":"node-a"}`, map[string]string{"Content-Type": "application/json", "X-Requested-With": "XMLHttpRequest"}, cookie); w.Code != http.StatusCreated {
		t.Fatalf("会话写操作带同源头应 201，实际 %d: %s", w.Code, w.Body.String())
	}

	// 管理令牌仍可直接调用写操作（自动化兼容）。
	if w = doAuthReq(h, http.MethodPost, "/api/admin/nodes", "10.10.0.9",
		`{"name":"node-b"}`, map[string]string{"Content-Type": "application/json", "X-Admin-Token": "web-test-admin-token-123456"}, nil); w.Code != http.StatusCreated {
		t.Fatalf("管理令牌应仍可写，实际 %d", w.Code)
	}

	w = doAuthReq(h, http.MethodPost, "/api/auth/logout", "10.10.0.10", "", nil, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("退出登录应 200，实际 %d", w.Code)
	}
	cleared := findSessionCookie(t, w)
	if cleared.MaxAge >= 0 {
		t.Errorf("退出登录应清除 Cookie（MaxAge<0），实际 %d", cleared.MaxAge)
	}
}

func TestAuthLoginAcceptsEmailCaseInsensitive(t *testing.T) {
	h, _ := newAuthWeb(t, time.Hour)
	w := loginReq(h, "10.11.0.1", "LOGIN@EXAMPLE.COM", testAdminPassword)
	if w.Code != http.StatusOK {
		t.Fatalf("邮箱大小写不敏感登录应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	if authed, _ := decodeSession(t, w); !authed {
		t.Fatal("邮箱登录应返回 authenticated=true")
	}
}

func TestAuthLoginFailuresAreRateLimited(t *testing.T) {
	h, _ := newAuthWeb(t, time.Hour)
	const ip = "10.12.0.1"
	for i := 0; i < 10; i++ {
		if w := loginReq(h, ip, "imotao", "wrong-password-123"); w.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次失败应 401，实际 %d", i+1, w.Code)
		}
	}
	if w := loginReq(h, ip, "imotao", "wrong-password-123"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("连续失败后应 429，实际 %d", w.Code)
	}
	if w := loginReq(h, "10.12.0.2", "imotao", testAdminPassword); w.Code != http.StatusOK {
		t.Fatalf("其他来源不应受牵连，实际 %d", w.Code)
	}
}

func TestAuthExpiredSessionRejected(t *testing.T) {
	h, _ := newAuthWeb(t, 20*time.Millisecond)
	w := loginReq(h, "10.13.0.1", "imotao", testAdminPassword)
	if w.Code != http.StatusOK {
		t.Fatalf("登录失败: %d %s", w.Code, w.Body.String())
	}
	cookie := findSessionCookie(t, w)
	time.Sleep(40 * time.Millisecond)

	w = doAuthReq(h, http.MethodGet, "/api/auth/session", "10.13.0.2", "", nil, cookie)
	if authed, _ := decodeSession(t, w); authed {
		t.Fatal("过期会话不应通过校验")
	}
	if w = doAuthReq(h, http.MethodGet, "/api/admin/nodes", "10.13.0.3", "", nil, cookie); w.Code != http.StatusUnauthorized {
		t.Fatalf("过期会话访问管理接口应 401，实际 %d", w.Code)
	}
}

// 登录成功应清零该来源的失败计数（之前若失败过几次，成功后不再被限速）。
func TestAuthLoginSuccessResetsFailures(t *testing.T) {
	h, _ := newAuthWeb(t, time.Hour)
	const ip = "10.14.0.1"
	for i := 0; i < 9; i++ {
		loginReq(h, ip, "imotao", "wrong-password-123")
	}
	if w := loginReq(h, ip, "imotao", testAdminPassword); w.Code != http.StatusOK {
		t.Fatalf("第 10 次登录（正确密码）应 200，实际 %d", w.Code)
	}
	for i := 0; i < 5; i++ {
		if w := loginReq(h, ip, "imotao", "wrong-password-123"); w.Code != http.StatusUnauthorized {
			t.Fatalf("成功登录后失败计数应清零，第 %d 次实际 %d", i+1, w.Code)
		}
	}
}

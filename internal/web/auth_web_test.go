package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/auth"
	"github.com/motao123/rollcall/internal/store"
)

const testAdminPassword = "correct-horse-battery-staple"

func newAuthWeb(t *testing.T, ttl time.Duration) (http.Handler, *auth.User) {
	t.Helper()
	h, _, u := newAuthWebAt(t, ttl)
	return h, u
}

func newAuthWebAt(t *testing.T, ttl time.Duration) (http.Handler, string, *auth.User) {
	t.Helper()
	dir := t.TempDir()
	userPath := filepath.Join(dir, "admin-user.json")
	u, created, generated, err := auth.LoadOrCreateUser(
		userPath,
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
		UserPath:   userPath,
		Sessions:   sessions,
	})
	return srv.Handler(), userPath, u
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

// sessionUser 是 /api/auth/session、/api/auth/login 与 /api/admin/account 下发的用户对象。
// 字段类型即断言：totpEnabled 必须是 JSON 布尔，用 map[string]string 解码会
// 把「服务端下发字符串 false」这种回归悄悄放过去。
type sessionUser struct {
	Username    string `json:"username"`
	Email       string `json:"email"`
	TOTPEnabled bool   `json:"totpEnabled"`
}

func decodeSession(t *testing.T, w *httptest.ResponseRecorder) (bool, sessionUser) {
	t.Helper()
	var got struct {
		Authenticated bool        `json:"authenticated"`
		User          sessionUser `json:"user"`
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

	// 来自可信代理（默认回环）且带 X-Forwarded-Proto: https 时，Cookie 必须标记 Secure。
	w = doAuthReq(h, http.MethodPost, "/api/auth/login", "127.0.0.1",
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
	if !authed || user.Username != "imotao" || user.Email != "login@example.com" {
		t.Fatalf("登录响应用户信息不符: %+v", user)
	}
	if user.TOTPEnabled {
		t.Fatalf("未启用两步验证时 totpEnabled 应为 false: %+v", user)
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

	// 登出后旧令牌必须在服务端失效：只清浏览器 Cookie 挡不住被复制的令牌。
	if w = doAuthReq(h, http.MethodGet, "/api/auth/session", "10.10.0.11", "", nil, cookie); w.Code != http.StatusOK {
		t.Fatalf("会话探测应保持 200，实际 %d", w.Code)
	} else if authed, _ := decodeSession(t, w); authed {
		t.Fatal("已登出的令牌不应再通过会话校验")
	}
	if w = doAuthReq(h, http.MethodGet, "/api/admin/nodes", "10.10.0.12", "", nil, cookie); w.Code != http.StatusUnauthorized {
		t.Fatalf("已登出的令牌访问管理接口应 401，实际 %d", w.Code)
	}
}

// TestAuthUntrustedForwardedProtoIgnored 直连客户端伪造 X-Forwarded-Proto
// 不得让 Cookie 带上 Secure：那会让真实 HTTP 浏览器丢弃会话，
// 也会误导后续的安全判断。
func TestAuthUntrustedForwardedProtoIgnored(t *testing.T) {
	h, _ := newAuthWeb(t, time.Hour)
	w := doAuthReq(h, http.MethodPost, "/api/auth/login", "203.0.113.9",
		`{"identifier":"imotao","password":"`+testAdminPassword+`"}`,
		map[string]string{"Content-Type": "application/json", "X-Forwarded-Proto": "https"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("登录失败: %d %s", w.Code, w.Body.String())
	}
	if c := findSessionCookie(t, w); c.Secure {
		t.Error("不可信来源的 X-Forwarded-Proto 不应影响 Cookie 的 Secure 标记")
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

// TestInitialPasswordFileRemovedAfterAuth 首启随机密码落盘后，
// 只有成功登录/改密才删除；失败登录不能把它删掉（否则用户再也拿不到密码）。
func TestInitialPasswordFileRemovedAfterAuth(t *testing.T) {
	h, userPath, _ := newAuthWebAt(t, time.Hour)
	p := filepath.Join(filepath.Dir(userPath), auth.InitialPasswordFile)
	write := func() {
		t.Helper()
		if err := os.WriteFile(p, []byte("bootstrap-password\n"), 0o600); err != nil {
			t.Fatalf("写入初始密码文件: %v", err)
		}
	}
	requireGone := func(stage string) {
		t.Helper()
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s 后应删除初始密码文件，stat err=%v", stage, err)
		}
	}

	write()
	if w := loginReq(h, "10.15.0.1", "imotao", "wrong-password-123"); w.Code != http.StatusUnauthorized {
		t.Fatalf("错误密码应 401，实际 %d", w.Code)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("失败登录不应删除初始密码文件: %v", err)
	}

	if w := loginReq(h, "10.15.0.2", "imotao", testAdminPassword); w.Code != http.StatusOK {
		t.Fatalf("登录失败: %d %s", w.Code, w.Body.String())
	}
	requireGone("成功登录")

	// 改密同样应清理（多实例/手工恢复可能留下旧文件）。
	write()
	login := loginReq(h, "10.15.0.3", "imotao", testAdminPassword)
	cookie := findSessionCookie(t, login)
	if w := doAuthReq(h, http.MethodPost, "/api/admin/account/password", "10.15.0.4",
		`{"currentPassword":"`+testAdminPassword+`","newPassword":"another-strong-password-1"}`,
		map[string]string{"Content-Type": "application/json", "X-Requested-With": "XMLHttpRequest"}, cookie); w.Code != http.StatusOK {
		t.Fatalf("修改密码失败: %d %s", w.Code, w.Body.String())
	}
	requireGone("修改密码")
}

// TestAdminAccountUpdateFlow 覆盖在线改账号/改密码：当前密码确认、
// CSRF 头、资料持久化、旧会话失效与当前会话续签。
func TestAdminAccountUpdateFlow(t *testing.T) {
	h, userPath, _ := newAuthWebAt(t, time.Hour)

	login := loginReq(h, "10.20.0.1", "imotao", testAdminPassword)
	if login.Code != http.StatusOK {
		t.Fatalf("登录失败: %d %s", login.Code, login.Body.String())
	}
	cookie := findSessionCookie(t, login)

	post := func(path, body, ip string, headers map[string]string, c *http.Cookie) *httptest.ResponseRecorder {
		hdr := map[string]string{"Content-Type": "application/json", "X-Requested-With": "XMLHttpRequest"}
		for k, v := range headers {
			hdr[k] = v
		}
		return doAuthReq(h, http.MethodPost, path, ip, body, hdr, c)
	}

	if w := doAuthReq(h, http.MethodPost, "/api/admin/account", "10.20.0.2",
		`{"username":"operator","email":"","currentPassword":"`+testAdminPassword+`"}`,
		map[string]string{"Content-Type": "application/json"}, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("未登录修改账号应 401，实际 %d", w.Code)
	}
	if w := doAuthReq(h, http.MethodPost, "/api/admin/account", "10.20.0.3",
		`{"username":"operator","email":"","currentPassword":"`+testAdminPassword+`"}`,
		map[string]string{"Content-Type": "application/json"}, cookie); w.Code != http.StatusForbidden {
		t.Fatalf("缺少同源头的会话写操作应 403，实际 %d", w.Code)
	}
	if w := post("/api/admin/account",
		`{"username":"operator","email":"","currentPassword":"wrong-password-123"}`, "10.20.0.4", nil, cookie); w.Code != http.StatusForbidden {
		t.Fatalf("当前密码错误应 403，实际 %d: %s", w.Code, w.Body.String())
	}
	if w := post("/api/admin/account",
		`{"username":"x","email":"","currentPassword":"`+testAdminPassword+`"}`, "10.20.0.5", nil, cookie); w.Code != http.StatusBadRequest {
		t.Fatalf("非法用户名应 400，实际 %d", w.Code)
	}
	if w := post("/api/admin/account",
		`{"username":"operator","email":"","currentPassword":"`+testAdminPassword+`","extra":1}`, "10.20.0.6", nil, cookie); w.Code != http.StatusBadRequest {
		t.Fatalf("未知字段应 400，实际 %d", w.Code)
	}

	w := post("/api/admin/account",
		`{"username":"operator","email":"Ops@Example.com","currentPassword":"`+testAdminPassword+`"}`, "10.20.0.7", nil, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("更新账号应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "passwordHash") {
		t.Fatal("账号响应不应包含密码哈希")
	}
	var updated struct {
		User sessionUser `json:"user"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &updated); err != nil {
		t.Fatalf("解析响应: %v (%s)", err, w.Body.String())
	}
	if updated.User.Username != "operator" || updated.User.Email != "ops@example.com" {
		t.Fatalf("更新后的账号信息不符: %+v", updated.User)
	}
	newCookie := findSessionCookie(t, w)

	// 旧会话版本已失效，当前浏览器换发的新 Cookie 可用。
	if w := doAuthReq(h, http.MethodGet, "/api/auth/session", "10.20.0.8", "", nil, cookie); w.Code != http.StatusOK {
		t.Fatalf("探测旧会话应 200，实际 %d", w.Code)
	} else if authed, _ := decodeSession(t, w); authed {
		t.Fatal("更新账号后旧会话应失效")
	}
	if w := doAuthReq(h, http.MethodGet, "/api/admin/nodes", "10.20.0.9", "", nil, cookie); w.Code != http.StatusUnauthorized {
		t.Fatalf("旧会话访问管理接口应 401，实际 %d", w.Code)
	}
	if w := doAuthReq(h, http.MethodGet, "/api/auth/session", "10.20.0.10", "", nil, newCookie); w.Code != http.StatusOK {
		t.Fatalf("续签会话应可用，实际 %d", w.Code)
	} else if authed, user := decodeSession(t, w); !authed || user.Username != "operator" {
		t.Fatalf("续签会话信息不符: authed=%v user=%+v", authed, user)
	}
	if w := loginReq(h, "10.20.0.11", "imotao", testAdminPassword); w.Code != http.StatusUnauthorized {
		t.Fatalf("旧用户名不应再能登录，实际 %d", w.Code)
	}
	if w := loginReq(h, "10.20.0.12", "operator", testAdminPassword); w.Code != http.StatusOK {
		t.Fatalf("新用户名应能用原密码登录，实际 %d", w.Code)
	}

	if w := post("/api/admin/account/password",
		`{"currentPassword":"`+testAdminPassword+`","newPassword":"short"}`, "10.20.0.13", nil, newCookie); w.Code != http.StatusBadRequest {
		t.Fatalf("过短新密码应 400，实际 %d", w.Code)
	}
	const newPassword = "new-admin-password-456"
	w = post("/api/admin/account/password",
		`{"currentPassword":"`+testAdminPassword+`","newPassword":"`+newPassword+`"}`, "10.20.0.14", nil, newCookie)
	if w.Code != http.StatusOK {
		t.Fatalf("修改密码应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	finalCookie := findSessionCookie(t, w)

	if w := doAuthReq(h, http.MethodGet, "/api/admin/nodes", "10.20.0.15", "", nil, newCookie); w.Code != http.StatusUnauthorized {
		t.Fatalf("改密前的会话应失效，实际 %d", w.Code)
	}
	if w := doAuthReq(h, http.MethodGet, "/api/auth/session", "10.20.0.16", "", nil, finalCookie); w.Code != http.StatusOK {
		t.Fatalf("改密后的会话应可用，实际 %d", w.Code)
	} else if authed, _ := decodeSession(t, w); !authed {
		t.Fatal("改密后当前浏览器应保持登录")
	}
	if w := loginReq(h, "10.20.0.17", "operator", testAdminPassword); w.Code != http.StatusUnauthorized {
		t.Fatalf("旧密码不应再能登录，实际 %d", w.Code)
	}
	if w := loginReq(h, "10.20.0.18", "operator", newPassword); w.Code != http.StatusOK {
		t.Fatalf("新密码应能登录，实际 %d", w.Code)
	}

	// 落盘结果必须重启后仍然有效。
	persisted, err := auth.LoadUser(userPath)
	if err != nil {
		t.Fatalf("重新加载账号文件: %v", err)
	}
	if persisted.Username != "operator" || persisted.Email != "ops@example.com" {
		t.Fatalf("资料未持久化: %+v", persisted)
	}
	if persisted.Authenticate("operator", testAdminPassword) || !persisted.Authenticate("operator", newPassword) {
		t.Fatal("密码未按预期持久化")
	}
}

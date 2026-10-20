package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/auth"
	"github.com/motao123/rollcall/internal/store"
)

// TestAccountSaveFailureDoesNotLeakPath 回归：
// saveUser 的错误是 *os.PathError，直接回显会把 admin-user.json 的
// 绝对路径与文件系统细节泄露给客户端。详情只应进日志。
func TestAccountSaveFailureDoesNotLeakPath(t *testing.T) {
	dir := t.TempDir()
	userPath := filepath.Join(dir, "admin-user.json")
	u, _, _, err := auth.LoadOrCreateUser(userPath, "imotao", "Login@Example.com", testAdminPassword, false)
	if err != nil {
		t.Fatalf("创建账号失败: %v", err)
	}
	sessions, err := auth.NewSessions(bytes.Repeat([]byte{0x42}, 32), time.Hour)
	if err != nil {
		t.Fatalf("创建会话管理器失败: %v", err)
	}

	// 把保存路径指向"父级是普通文件"的位置，写入必然失败（模拟磁盘/权限异常）
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("准备阻断文件失败: %v", err)
	}
	badPath := filepath.Join(blocker, "admin-user.json")

	srv := New(store.New(t.TempDir()), nil, nil, Config{
		AdminToken: "hardening-admin-token-123456",
		User:       u,
		UserPath:   badPath,
		Sessions:   sessions,
	})
	h := srv.Handler()

	login := loginReq(h, "10.40.0.1", "imotao", testAdminPassword)
	if login.Code != http.StatusOK {
		t.Fatalf("登录失败: %d %s", login.Code, login.Body.String())
	}
	cookie := findSessionCookie(t, login)

	body := `{"currentPassword":` + mustJSON(testAdminPassword) + `,"newPassword":"another-passphrase-2026"}`
	w := doAuthReq(h, http.MethodPost, "/api/admin/account/password", "10.40.0.1", body,
		map[string]string{"Content-Type": "application/json", "X-Requested-With": "XMLHttpRequest"}, cookie)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("保存失败应 500，实际 %d: %s", w.Code, w.Body.String())
	}
	resp := w.Body.String()
	if strings.Contains(resp, "blocker") || strings.Contains(resp, dir) {
		t.Fatalf("响应泄露了内部路径: %s", resp)
	}
	if strings.Contains(resp, "not a directory") || strings.Contains(resp, "no such file") {
		t.Fatalf("响应泄露了文件系统错误细节: %s", resp)
	}
	if !strings.Contains(resp, "保存密码失败") {
		t.Errorf("应给出通用错误文案，实际: %s", resp)
	}
}

// TestTOTPChangeViaAPITokenSetsNoCookie 回归：
// 用 X-Admin-Token 调用 TOTP 启停时不属于浏览器会话，
// 不应收到 Set-Cookie（旧实现无条件 reissue=true）。
func TestTOTPChangeViaAPITokenSetsNoCookie(t *testing.T) {
	h, _ := newAuthWeb(t, time.Hour)
	const tok = "web-test-admin-token-123456"
	const ip = "10.41.0.1"
	hdr := map[string]string{"Content-Type": "application/json", "X-Requested-With": "XMLHttpRequest", "X-Admin-Token": tok}

	w := doAuthReq(h, http.MethodPost, "/api/admin/account/totp", ip,
		`{"action":"setup","currentPassword":`+mustJSON(testAdminPassword)+`}`, hdr, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("setup 失败: %d %s", w.Code, w.Body.String())
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
		`{"action":"enable","currentPassword":`+mustJSON(testAdminPassword)+`,"code":"`+code+`"}`, hdr, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("enable 失败: %d %s", w.Code, w.Body.String())
	}
	if cookies := w.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("令牌调用不应下发会话 Cookie，实际 %d 个: %v", len(cookies), cookies)
	}
}

// TestEnableTOTPKeepsBrowserSession 回归：
// 启用 2FA 会递增会话版本。若"是否来自浏览器会话"在版本递增之后才判断，
// 请求里的旧 Cookie 已失配 → 服务端不下发新 Cookie → 用户刚启用 2FA 就被登出。
// 这里断言启用后换发的 Cookie 仍然有效。
func TestEnableTOTPKeepsBrowserSession(t *testing.T) {
	h, _ := newAuthWeb(t, time.Hour)
	const ip = "10.42.0.1"
	_, cookie := enableTOTP(t, h, ip)
	if cookie == nil {
		t.Fatal("启用 2FA 后应换发会话 Cookie")
	}
	w := doAuthReq(h, http.MethodGet, "/api/auth/session", ip, "", nil, cookie)
	var sess struct {
		Authenticated bool `json:"authenticated"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &sess); err != nil {
		t.Fatalf("解析会话响应失败: %v", err)
	}
	if !sess.Authenticated {
		t.Fatalf("启用 2FA 后会话应保持有效，实际: %s", w.Body.String())
	}
}

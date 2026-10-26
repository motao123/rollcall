package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/motao123/rollcall/internal/store"
)

func TestNormalizePanelLink(t *testing.T) {
	ok := []struct{ in, want string }{
		{"https://monitor.example.com", "https://monitor.example.com"},
		{"https://monitor.example.com/", "https://monitor.example.com"},
		{"  http://203.0.113.10:19080  ", "http://203.0.113.10:19080"},
		{"https://monitor.example.com:8443", "https://monitor.example.com:8443"},
		{"", ""},
	}
	for _, c := range ok {
		got, msg := normalizePanelLink(c.in)
		if msg != "" || got != c.want {
			t.Fatalf("%q → %q (%s)，期望 %q", c.in, got, msg, c.want)
		}
	}

	bad := []struct{ in, wantSub string }{
		{"monitor.example.com", "http"},                  // 缺 scheme
		{"ftp://monitor.example.com", "http"},            // 非 http(s)
		{"javascript:alert(1)", "http"},                  // 危险 scheme
		{"https://monitor.example.com/rollcall/", "子路径"}, // 子路径部署不支持
		{"https://monitor.example.com/?x=1", "查询参数"},     // 带 query
		{"https:///nohost", "主机名"},                       // 缺 host
	}
	for _, c := range bad {
		if _, msg := normalizePanelLink(c.in); !strings.Contains(msg, c.wantSub) {
			t.Fatalf("%q 应被拒绝并提示 %q，实际 %q", c.in, c.wantSub, msg)
		}
	}
}

// TestPanelLinkEndpointPersists 覆盖「面板里改地址」的完整链路：
// 写入 → 落盘 → 重启（新建 Server）后仍然生效。
//
// 这条比只测 handler 重要：字段如果只存在内存里，重启后又变回空，
// 告警链接会静默失效。
func TestPanelLinkEndpointPersists(t *testing.T) {
	dir := t.TempDir()
	st := store.New(dir)
	srv := New(st, nil, nil, Config{AdminToken: "test-admin-token-1234567890"})

	body := `{"link":"https://monitor.example.com/"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/settings/panel-link", strings.NewReader(body))
	req.Header.Set("X-Admin-Token", "test-admin-token-1234567890")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["panelLink"] != "https://monitor.example.com" {
		t.Fatalf("应规范化为无尾斜杠，实际 %q", resp["panelLink"])
	}
	if got := st.GetSetting("panel_link"); got != "https://monitor.example.com" {
		t.Fatalf("应落盘到设置，实际 %q", got)
	}

	// 模拟重启：先落盘（真实重启会走这一步），再新建 Server 读回。
	if err := st.Save(); err != nil {
		t.Fatalf("落盘失败: %v", err)
	}
	srv2 := New(store.New(dir), nil, nil, Config{AdminToken: "test-admin-token-1234567890"})
	if got := srv2.PanelLink(); got != "https://monitor.example.com" {
		t.Fatalf("重启后应读回已保存的地址，实际 %q", got)
	}

	// 空值 = 清除（回落到「用当前访问地址」）。
	req = httptest.NewRequest(http.MethodPost, "/api/admin/settings/panel-link", strings.NewReader(`{"link":""}`))
	req.Header.Set("X-Admin-Token", "test-admin-token-1234567890")
	w = httptest.NewRecorder()
	srv2.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("清空应 200，实际 %d", w.Code)
	}
	if got := srv2.PanelLink(); got != "" {
		t.Fatalf("清空后应为空，实际 %q", got)
	}
}

// TestPanelLinkRejectsAndRequiresAuth 覆盖两类失败：错误输入与未鉴权。
func TestPanelLinkRejectsAndRequiresAuth(t *testing.T) {
	srv := New(store.New(t.TempDir()), nil, nil, Config{AdminToken: "test-admin-token-1234567890"})

	req := httptest.NewRequest(http.MethodPost, "/api/admin/settings/panel-link",
		strings.NewReader(`{"link":"https://monitor.example.com/sub/"}`))
	req.Header.Set("X-Admin-Token", "test-admin-token-1234567890")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("子路径应 400，实际 %d: %s", w.Code, w.Body.String())
	}

	// 未带令牌：不能被写入。
	req = httptest.NewRequest(http.MethodPost, "/api/admin/settings/panel-link",
		strings.NewReader(`{"link":"https://evil.example.com"}`))
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Fatal("缺少管理令牌时必须拒绝")
	}
	if got := srv.PanelLink(); got != "" {
		t.Fatalf("未鉴权请求不应写入，实际 %q", got)
	}
}

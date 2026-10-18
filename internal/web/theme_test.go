package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/motao/rollcall/internal/store"
)

const themeTestToken = "test-admin-token-1234567890"

func doThemeReq(t *testing.T, srv *Server, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("X-Admin-Token", token)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func TestThemePublicAndAdmin(t *testing.T) {
	st := store.New(t.TempDir())
	srv := New(st, nil, nil, Config{AdminToken: themeTestToken})

	// 公开端点不需要令牌，且默认是 auto。
	w := doThemeReq(t, srv, http.MethodGet, "/api/theme", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("公开主题接口应 200，实际 %d", w.Code)
	}
	var pub map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &pub); err != nil {
		t.Fatalf("解析公开主题: %v", err)
	}
	if pub["mode"] != "auto" {
		t.Fatalf("默认模式应为 auto: %+v", pub)
	}

	// 管理端写操作必须带令牌。
	if w := doThemeReq(t, srv, http.MethodPut, "/api/admin/theme", "", `{"mode":"dark"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("无令牌保存应 401，实际 %d", w.Code)
	}

	// 合法保存并回读。
	body := `{"mode":"light","accent":"#ABCDEF","accentDark":"#123456","radius":6,"compact":true}`
	if w := doThemeReq(t, srv, http.MethodPut, "/api/admin/theme", themeTestToken, body); w.Code != http.StatusOK {
		t.Fatalf("保存主题应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	w = doThemeReq(t, srv, http.MethodGet, "/api/theme", "", "")
	if !strings.Contains(w.Body.String(), `"accent":"#abcdef"`) {
		t.Fatalf("公开接口未返回归一化后的主题: %s", w.Body.String())
	}

	// 未知字段 / CSS 文本 / 非法模式必须拒绝，防止主题变成样式注入入口。
	rejects := []string{
		`{"mode":"light","css":"body{display:none}"}`,
		`{"accent":"red;background:url(x)"}`,
		`{"mode":"neon"}`,
		`{"radius":99}`,
	}
	for _, b := range rejects {
		if w := doThemeReq(t, srv, http.MethodPut, "/api/admin/theme", themeTestToken, b); w.Code != http.StatusBadRequest {
			t.Fatalf("%s 应 400，实际 %d", b, w.Code)
		}
	}

	// 损坏的已保存配置不应让公开接口 500。
	st.SetSetting("theme", "not-json")
	w = doThemeReq(t, srv, http.MethodGet, "/api/theme", "", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"mode":"auto"`) {
		t.Fatalf("损坏配置应回退默认主题，实际 %d: %s", w.Code, w.Body.String())
	}
}

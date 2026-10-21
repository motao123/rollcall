package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/motao123/rollcall/internal/store"
)

func writeThemePack(t *testing.T, dataDir, name, title, css, js string) {
	t.Helper()
	dir := filepath.Join(dataDir, "themes", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"title":"` + title + `","version":"1.0"}`
	if err := os.WriteFile(filepath.Join(dir, "theme.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if css != "" {
		if err := os.WriteFile(filepath.Join(dir, "status.css"), []byte(css), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if js != "" {
		if err := os.WriteFile(filepath.Join(dir, "status.js"), []byte(js), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestThemePackAdminAndStatusPage(t *testing.T) {
	dataDir := t.TempDir()
	st := store.New(dataDir)
	srv := New(st, nil, nil, Config{AdminToken: themeTestToken, DataDir: dataDir})
	writeThemePack(t, dataDir, "aurora", "极光", "html{--accent:#f0f}", "document.documentElement.dataset.pack='aurora'")
	writeThemePack(t, dataDir, "empty", "空包", "", "")

	// 未选择主题时，公开页保持内置外观。
	w := doThemeReq(t, srv, http.MethodGet, "/status", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("/status: %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "data-theme-pack") {
		t.Fatal("未选择主题包时不应注入主题")
	}

	// 管理列表：有效包带元数据，无效包带原因。
	w = doThemeReq(t, srv, http.MethodGet, "/api/admin/theme-packs", themeTestToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("列表: %d %s", w.Code, w.Body.String())
	}
	var list struct {
		Active string          `json:"active"`
		Dir    string          `json:"dir"`
		Packs  []themePackView `json:"packs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("解析列表: %v", err)
	}
	if list.Active != "" || list.Dir != filepath.Join(dataDir, "themes") || len(list.Packs) != 2 {
		t.Fatalf("列表异常: %+v", list)
	}
	for _, p := range list.Packs {
		if p.Name == "empty" && p.Error == "" {
			t.Fatalf("空包应带错误: %+v", p)
		}
		if p.Name == "aurora" && (p.Title != "极光" || p.Error != "" || !p.HasCSS || !p.HasJS) {
			t.Fatalf("aurora 异常: %+v", p)
		}
	}

	// 写操作需要管理权限。
	if w := doThemeReq(t, srv, http.MethodPut, "/api/admin/theme-packs", "", `{"name":"aurora"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("未授权选择应 401，实际 %d", w.Code)
	}
	// 不存在或无效的包必须当场拒绝，不能等到公开页静默回退。
	if w := doThemeReq(t, srv, http.MethodPut, "/api/admin/theme-packs", themeTestToken, `{"name":"missing"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("不存在的主题应 400，实际 %d", w.Code)
	}
	if w := doThemeReq(t, srv, http.MethodPut, "/api/admin/theme-packs", themeTestToken, `{"name":"empty"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("无效主题应 400，实际 %d", w.Code)
	}
	if w := doThemeReq(t, srv, http.MethodPut, "/api/admin/theme-packs", themeTestToken, `{"name":"aurora","x":1}`); w.Code != http.StatusBadRequest {
		t.Fatalf("未知字段应 400，实际 %d", w.Code)
	}
	if w := doThemeReq(t, srv, http.MethodPut, "/api/admin/theme-packs", themeTestToken, `{"name":"aurora"}`); w.Code != http.StatusOK {
		t.Fatalf("选择主题: %d %s", w.Code, w.Body.String())
	}
	if st.GetSetting(themePackSetting) != "aurora" {
		t.Fatalf("选择未写入设置: %q", st.GetSetting(themePackSetting))
	}

	// 公开页注入主题资源，且主题脚本仍受 script-src 'self' 约束。
	w = doThemeReq(t, srv, http.MethodGet, "/status", "", "")
	page := w.Body.String()
	for _, want := range []string{
		`data-theme-pack="aurora"`,
		`<link rel="stylesheet" href="/themes/aurora/status.css">`,
		`<script src="/themes/aurora/status.js"></script>`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("公开页缺少 %q", want)
		}
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("CSP 异常: %q", w.Header().Get("Content-Security-Policy"))
	}

	// 资源下发：固定文件名、no-cache、可 HEAD。
	w = doThemeReq(t, srv, http.MethodGet, "/themes/aurora/status.css", "", "")
	if w.Code != http.StatusOK || w.Body.String() != "html{--accent:#f0f}" {
		t.Fatalf("CSS: %d %q", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Fatalf("CSS Content-Type: %q", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("CSS Cache-Control: %q", cc)
	}
	w = doThemeReq(t, srv, http.MethodGet, "/themes/aurora/status.js", "", "")
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/javascript") {
		t.Fatalf("JS: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	if w := doThemeReq(t, srv, http.MethodHead, "/themes/aurora/status.css", "", ""); w.Code != http.StatusOK {
		t.Fatalf("HEAD: %d", w.Code)
	}
	if w := doThemeReq(t, srv, http.MethodPost, "/themes/aurora/status.css", "", ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST 应 405，实际 %d", w.Code)
	}

	// 只下发白名单资源，且主题名与文件名都不能拼出目录穿越。
	for _, path := range []string{
		"/themes/aurora/theme.json",
		"/themes/aurora/missing.css",
		"/themes/missing/status.css",
		"/themes/Aurora/status.css",
		"/themes/aurora/sub/status.css",
	} {
		if w := doThemeReq(t, srv, http.MethodGet, path, "", ""); w.Code != http.StatusNotFound {
			t.Fatalf("%s 应 404，实际 %d", path, w.Code)
		}
	}
	for _, path := range []string{"/themes/../status.css", "/themes/aurora/../status.css", "/themes/aurora/..%2fsecret"} {
		req := httptest.NewRequest(http.MethodGet, "/themes/aurora/status.css", nil)
		req.URL.Path = path
		rec := httptest.NewRecorder()
		srv.handleStatusThemeAsset(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s 应 404，实际 %d", path, rec.Code)
		}
	}

	// 清空选择后回到内置外观。
	if w := doThemeReq(t, srv, http.MethodPut, "/api/admin/theme-packs", themeTestToken, `{"name":""}`); w.Code != http.StatusOK {
		t.Fatalf("清空: %d %s", w.Code, w.Body.String())
	}
	if page := doThemeReq(t, srv, http.MethodGet, "/status", "", "").Body.String(); strings.Contains(page, "data-theme-pack") {
		t.Fatal("清空后不应再注入主题")
	}

	// 已选择的主题被删坏：公开页回退，管理端给出明确警示。
	if w := doThemeReq(t, srv, http.MethodPut, "/api/admin/theme-packs", themeTestToken, `{"name":"aurora"}`); w.Code != http.StatusOK {
		t.Fatalf("重新选择: %d", w.Code)
	}
	if err := os.Remove(filepath.Join(dataDir, "themes", "aurora", "theme.json")); err != nil {
		t.Fatal(err)
	}
	if page := doThemeReq(t, srv, http.MethodGet, "/status", "", "").Body.String(); strings.Contains(page, "data-theme-pack") {
		t.Fatal("损坏的主题包应回退为内置外观")
	}
	w = doThemeReq(t, srv, http.MethodGet, "/api/admin/theme-packs", themeTestToken, "")
	var broken struct {
		Warning string `json:"warning"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &broken); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(broken.Warning, "回退为内置主题") {
		t.Fatalf("应提示主题包不可用: %+v", broken)
	}
	if w := doThemeReq(t, srv, http.MethodGet, "/themes/aurora/status.css", "", ""); w.Code != http.StatusNotFound {
		t.Fatalf("损坏主题的资源应 404，实际 %d", w.Code)
	}
}

func TestThemePackWithoutDataDir(t *testing.T) {
	st := store.New(t.TempDir())
	srv := New(st, nil, nil, Config{AdminToken: themeTestToken})
	w := doThemeReq(t, srv, http.MethodGet, "/api/admin/theme-packs", themeTestToken, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"packs":[]`) {
		t.Fatalf("无数据目录时应返回空列表: %d %s", w.Code, w.Body.String())
	}
	if w := doThemeReq(t, srv, http.MethodGet, "/themes/aurora/status.css", "", ""); w.Code != http.StatusNotFound {
		t.Fatalf("无数据目录时资源应 404，实际 %d", w.Code)
	}
}

// 设置页的主题包入口容易被后续改版删掉，这里固定住前端接线。
func TestThemePackFrontendWiring(t *testing.T) {
	b, err := assets.ReadFile("dist/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	for _, want := range []string{
		"/api/admin/theme-packs",
		"themePackForm",
		"公开状态页主题包",
		"安装主题等同授予管理员权限",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js 缺少主题包接线: %q", want)
		}
	}
}

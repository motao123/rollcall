package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/store"
)

// publicStore 返回一个"公开页已打开"的存储，供只关心页面内容、不测门槛的用例使用。
// v1.15.2 起公开页默认关闭，涉及 /status 与 /api/status* 的用例必须显式打开。
func publicStore(t *testing.T) *store.Store {
	t.Helper()
	st := store.New(t.TempDir())
	enablePublicStatus(st)
	return st
}

// enablePublicStatus 在测试里显式打开公开页（默认是关闭的）。
func enablePublicStatus(st *store.Store) {
	on := true
	cfg := st.StatusPage()
	cfg.Enabled = &on
	st.SetStatusPage(cfg)
}

// gateEnv 起一个"公开页配置由用例决定"的服务端。
func gateEnv(t *testing.T, mutate func(*model.StatusPage)) (http.Handler, *store.Store) {
	t.Helper()
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "公开机", Token: "tok1", Group: "prod",
		Note: "内部备注：月付 49.9 美元"})
	st.UpsertNode(&model.Node{ID: "n2", Name: "隐藏机", Token: "tok2", Group: "prod"})
	cfg := st.StatusPage()
	if mutate != nil {
		mutate(&cfg)
	}
	st.SetStatusPage(cfg)
	srv := New(st, nil, nil, Config{AdminToken: "gate-test-admin-token-123456"})
	return srv.Handler(), st
}

func get(h http.Handler, path string, headers map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestPublicStatusDisabledStays404 覆盖显式关闭的语义：管理员关掉总开关后
// 依然是一律 404（不回 403，避免向扫描器暴露"这里确实有个页面"）。
// v1.25.0 起默认开启（见 DefaultStatusPage），默认路径由
// TestPublicStatusOpenByDefault 覆盖。
func TestPublicStatusDisabledStays404(t *testing.T) {
	h, _ := gateEnv(t, func(c *model.StatusPage) {
		off := false
		c.Enabled = &off
	})

	for _, path := range []string{"/status", "/status.css", "/api/status", "/vendor/element-plus/index.css"} {
		if w := get(h, path, nil); w.Code != http.StatusNotFound {
			t.Fatalf("%s 显式关闭后应 404，实际 %d", path, w.Code)
		}
	}
	// API 路径要回 JSON，前端才不会解析 HTML 报错
	w := get(h, "/api/status", nil)
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("API 404 应回 JSON，实际 %q", ct)
	}
}

// TestPublicStatusOpenByDefault v1.25.0 产品决策：状态页默认开启。
// 装好即可看到（脱敏保证不变：无 IP/价格/到期日/主机指纹）；
// 不想公开的实例显式关闭（上面的测试）或配令牌。
func TestPublicStatusOpenByDefault(t *testing.T) {
	h, _ := gateEnv(t, nil) // 不做任何配置：默认值生效

	for _, path := range []string{"/status", "/status.css", "/api/status"} {
		if w := get(h, path, nil); w.Code != http.StatusOK {
			t.Fatalf("%s 默认应 200（v1.25.0 起默认开启），实际 %d", path, w.Code)
		}
	}
	// 公开 API 必须仍是脱敏形状（enablePublicStatus 的既有测试覆盖字段级，
	// 这里确认默认路径与显式开启走的是同一条放行逻辑）。
	w := get(h, "/api/status", nil)
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("公开 API 应回 JSON，实际 %q", ct)
	}
}

// TestPublicStatusTokenGate 覆盖令牌门槛：查询串、Cookie、请求头三条路径，
// 以及"令牌只在命中时种 Cookie"。
func TestPublicStatusTokenGate(t *testing.T) {
	const tok = "kQ9zXb2LmN4pR7sT"
	h, _ := gateEnv(t, func(c *model.StatusPage) {
		on := true
		c.Enabled = &on
		c.Token = tok
	})

	if w := get(h, "/api/status", nil); w.Code != http.StatusNotFound {
		t.Fatalf("不带令牌应 404，实际 %d", w.Code)
	}
	if w := get(h, "/api/status?t=wrong-token-value", nil); w.Code != http.StatusNotFound {
		t.Fatalf("错误令牌应 404，实际 %d", w.Code)
	}

	w := get(h, "/api/status?t="+tok, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("正确令牌应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == publicStatusCookie {
			cookie = c
		}
	}
	if cookie == nil || cookie.Value != tok || !cookie.HttpOnly {
		t.Fatalf("命中令牌后应种 HttpOnly Cookie，实际 %+v", cookie)
	}

	// 带上 Cookie 后，页面里的静态资源与 SSE 不必重复带查询串
	if w := get(h, "/api/status", nil, cookie); w.Code != http.StatusOK {
		t.Fatalf("带 Cookie 应 200，实际 %d", w.Code)
	}
	if w := get(h, "/api/status", map[string]string{"X-Status-Token": tok}); w.Code != http.StatusOK {
		t.Fatalf("带 X-Status-Token 应 200，实际 %d", w.Code)
	}
	if w := get(h, "/api/status", map[string]string{"Authorization": "Bearer " + tok}); w.Code != http.StatusOK {
		t.Fatalf("带 Bearer 应 200，实际 %d", w.Code)
	}
	// 错误令牌不应被写进浏览器
	w = get(h, "/api/status?t=wrong-token-value", nil)
	for _, c := range w.Result().Cookies() {
		if c.Name == publicStatusCookie {
			t.Fatalf("错误令牌不应种 Cookie: %+v", c)
		}
	}
}

// TestPublicStatusHiddenNodesAndNotes 覆盖逐台隐藏与公开备注：
// 隐藏节点既不进列表也不能查详情；公开备注只来自 PublicNotes，绝不复用内部 Note。
func TestPublicStatusHiddenNodesAndNotes(t *testing.T) {
	h, st := gateEnv(t, func(c *model.StatusPage) {
		on := true
		c.Enabled = &on
		c.HiddenNodes = []string{"n2"}
		c.PublicNotes = map[string]string{"n1": "香港 · 家宽"}
	})

	w := get(h, "/api/status", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "隐藏机") {
		t.Fatalf("隐藏节点不应出现在列表里: %s", body)
	}
	if !strings.Contains(body, "香港 · 家宽") {
		t.Fatalf("公开备注应下发: %s", body)
	}
	if strings.Contains(body, "月付 49.9") {
		t.Fatalf("内部 Note 绝不能出现在公开页: %s", body)
	}

	// 细节：隐藏节点即使知道句柄也拿不到
	if w := get(h, "/api/status/node/"+st.PublicHandle("n2"), nil); w.Code != http.StatusNotFound {
		t.Fatalf("隐藏节点的详情应 404，实际 %d", w.Code)
	}
	if w := get(h, "/api/status/node/"+st.PublicHandle("n1"), nil); w.Code != http.StatusOK {
		t.Fatalf("未隐藏节点详情应 200，实际 %d", w.Code)
	}
}

// TestAdminStatusGateValidation 覆盖写入侧的校验：
// 令牌形状、未知节点 ID 都必须被拒绝，而不是静默接受后让人以为生效了。
func TestAdminStatusGateValidation(t *testing.T) {
	h, _ := gateEnv(t, nil)
	const admin = "gate-test-admin-token-123456"

	put := func(body string) int {
		req := httptest.NewRequest(http.MethodPut, "/api/admin/status-page", strings.NewReader(body))
		req.Header.Set("X-Admin-Token", admin)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}

	if code := put(`{"enabled":true,"token":"short"}`); code != http.StatusBadRequest {
		t.Fatalf("过短令牌应 400，实际 %d", code)
	}
	if code := put(`{"enabled":true,"token":"has space and $pecial chars"}`); code != http.StatusBadRequest {
		t.Fatalf("含非法字符的令牌应 400，实际 %d", code)
	}
	if code := put(`{"enabled":true,"hiddenNodes":["nope"]}`); code != http.StatusBadRequest {
		t.Fatalf("未知节点 ID 应 400，实际 %d", code)
	}
	if code := put(`{"enabled":true,"publicNotes":{"nope":"x"}}`); code != http.StatusBadRequest {
		t.Fatalf("未知节点的公开备注应 400，实际 %d", code)
	}
	if code := put(`{"enabled":true,"hiddenNodes":["n2"],"publicNotes":{"n1":"香港"}}`); code != http.StatusOK {
		t.Fatalf("合法配置应 200，实际 %d", code)
	}

	// 轮换令牌：旧令牌立刻失效
	req := httptest.NewRequest(http.MethodPost, "/api/admin/status-page/token", nil)
	req.Header.Set("X-Admin-Token", admin)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("轮换令牌应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Token == "" {
		t.Fatalf("轮换令牌应返回新值: %s", w.Body.String())
	}
}

// TestAdminStatusGateClearsNotes 回归：{"publicNotes":{}} 必须真的清空。
// json.Unmarshal 解到已存在的 map 上是合并而不是替换，曾经让"删掉备注保存后
// 备注仍在"；同时验证"请求体没带这个键时保持原值"。
func TestAdminStatusGateClearsNotes(t *testing.T) {
	h, st := gateEnv(t, func(c *model.StatusPage) {
		on := true
		c.Enabled = &on
		c.PublicNotes = map[string]string{"n1": "旧备注"}
		c.HiddenNodes = []string{"n2"}
	})
	const admin = "gate-test-admin-token-123456"

	put := func(body string) int {
		req := httptest.NewRequest(http.MethodPut, "/api/admin/status-page", strings.NewReader(body))
		req.Header.Set("X-Admin-Token", admin)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}

	// 没带 publicNotes：保留
	if code := put(`{"enabled":true}`); code != http.StatusOK {
		t.Fatalf("应 200，实际 %d", code)
	}
	if got := st.StatusPage().PublicNote("n1"); got != "旧备注" {
		t.Fatalf("未提交的备注应保留，实际 %q", got)
	}
	// 显式给空对象：清空
	if code := put(`{"enabled":true,"publicNotes":{},"hiddenNodes":[]}`); code != http.StatusOK {
		t.Fatalf("应 200，实际 %d", code)
	}
	if got := st.StatusPage().PublicNote("n1"); got != "" {
		t.Fatalf("空对象应清空备注，实际 %q", got)
	}
	if n := len(st.StatusPage().HiddenNodes); n != 0 {
		t.Fatalf("空数组应清空隐藏列表，实际 %d", n)
	}
}

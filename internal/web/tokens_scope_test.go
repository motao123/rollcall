package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/store"
)

// TestCreateAPITokenRejectsUnknownScope 是 P3 回归：
// 未知作用域曾被静默丢弃，{"scopes":["read","write"]} 会签发只有 read 的令牌，
// 调用方以为拿到了写权限，直到某天才收到莫名的 401。
func TestCreateAPITokenRejectsUnknownScope(t *testing.T) {
	const tok = "scope-test-admin-token-123456"
	srv := New(store.New(t.TempDir()), nil, nil, Config{AdminToken: tok})
	h := srv.Handler()

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/admin/tokens", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Admin-Token", tok)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	w := post(`{"name":"ci","scopes":["read","write"]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("含未知作用域应 400，实际 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "无效的权限范围") {
		t.Errorf("错误信息应说明作用域无效，实际: %s", w.Body.String())
	}

	// 合法作用域仍然可用
	if w := post(`{"name":"ci","scopes":["read"]}`); w.Code >= 300 {
		t.Fatalf("合法作用域应成功，实际 %d: %s", w.Code, w.Body.String())
	}
}

// TestAPITokenPathScopeAndShortExpiry 覆盖路径限定与分钟级有效期：
// 路径按段边界匹配（n1 不放行 n10），短期令牌到期即拒绝，非法参数整单拒绝。
func TestAPITokenPathScopeAndShortExpiry(t *testing.T) {
	const adminTok = "path-scope-admin-token-123456"
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "节点一", Token: "node-token"})
	h := New(st, nil, nil, Config{AdminToken: adminTok}).Handler()

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/admin/tokens", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Admin-Token", adminTok)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	create := func(body string) (string, model.APIToken) {
		t.Helper()
		w := post(body)
		if w.Code != http.StatusCreated {
			t.Fatalf("创建令牌失败 %d: %s", w.Code, w.Body.String())
		}
		var out struct {
			Token string         `json:"token"`
			Meta  model.APIToken `json:"meta"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("解析创建响应: %v", err)
		}
		return out.Token, out.Meta
	}

	raw, meta := create(`{"name":"概览只读","scopes":["read"],"paths":["/api/overview"],"expiresMinutes":30}`)
	want := time.Now().Unix() + 30*60
	if meta.ExpiresAt < want-90 || meta.ExpiresAt > want+90 {
		t.Errorf("expiresMinutes 未生效: got %d want≈%d", meta.ExpiresAt, want)
	}
	if len(meta.Paths) != 1 || meta.Paths[0] != "/api/overview" {
		t.Errorf("路径限定未保存: %+v", meta.Paths)
	}
	if w := bearerReq(h, http.MethodGet, "/api/overview", raw, ""); w.Code != http.StatusOK {
		t.Errorf("白名单路径应放行，实际 %d", w.Code)
	}
	if w := bearerReq(h, http.MethodGet, "/api/nodes", raw, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("白名单外路径应拒绝，实际 %d", w.Code)
	}

	// 节点级限定：/api/nodes/n1 覆盖详情与子路径，但不能越界到 n10。
	rawNode, _ := create(`{"name":"单节点","scopes":["read"],"paths":["/api/nodes/n1"],"expiresMinutes":60}`)
	if w := bearerReq(h, http.MethodGet, "/api/nodes/n1", rawNode, ""); w.Code != http.StatusOK {
		t.Errorf("节点详情应放行，实际 %d: %s", w.Code, w.Body.String())
	}
	if w := bearerReq(h, http.MethodGet, "/api/nodes/n1/series?range=1h", rawNode, ""); w.Code != http.StatusOK {
		t.Errorf("节点子路径应放行，实际 %d: %s", w.Code, w.Body.String())
	}
	for _, path := range []string{"/api/nodes", "/api/nodes/n10"} {
		if w := bearerReq(h, http.MethodGet, path, rawNode, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("%s 应拒绝，实际 %d", path, w.Code)
		}
	}
	// 含 .. 的路径先被 ServeMux 重定向；跟随到清理后的 n10 仍必须被拒绝。
	if w := bearerReq(h, http.MethodGet, "/api/nodes/n1/../n10", rawNode, ""); w.Code != http.StatusTemporaryRedirect {
		t.Errorf("越界路径应被重定向，实际 %d", w.Code)
	}
	if w := bearerReq(h, http.MethodGet, "/api/nodes/n10", rawNode, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("重定向后的越界路径应拒绝，实际 %d", w.Code)
	}

	// 末尾斜杠与重复项会被归一化。
	_, metaNorm := create(`{"name":"归一化","scopes":["read"],"paths":["/api/nodes/n1/","/api/nodes/n1"]}`)
	if len(metaNorm.Paths) != 1 || metaNorm.Paths[0] != "/api/nodes/n1" {
		t.Errorf("路径未归一化: %+v", metaNorm.Paths)
	}

	// 已过期令牌立即失效。
	now := time.Now().Unix()
	st.AddAPIToken(&model.APIToken{
		ID: "tk-expired", Name: "过期", Prefix: "rct_expired", Hash: hashAPIToken("rct_expired"),
		Scopes: []string{"read"}, CreatedAt: now, ExpiresAt: now - 1,
	})
	if w := bearerReq(h, http.MethodGet, "/api/overview", "rct_expired", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("过期令牌应拒绝，实际 %d", w.Code)
	}

	for _, body := range []string{
		`{"name":"x","scopes":["read"],"paths":["overview"]}`,
		`{"name":"x","scopes":["read"],"paths":["/api/../etc"]}`,
		`{"name":"x","scopes":["read"],"paths":["/api/nodes?x=1"]}`,
		`{"name":"x","scopes":["read"],"paths":[""]}`,
		`{"name":"x","scopes":["read"],"expiresDays":1,"expiresMinutes":60}`,
		`{"name":"x","scopes":["read"],"expiresMinutes":5256001}`,
	} {
		if w := post(body); w.Code != http.StatusBadRequest {
			t.Errorf("非法请求 %s 应 400，实际 %d: %s", body, w.Code, w.Body.String())
		}
	}
}

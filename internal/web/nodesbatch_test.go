package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/motao/rollcall/internal/model"
	"github.com/motao/rollcall/internal/store"
)

// batchEnv 两台节点 + 管理头。
func batchEnv(t *testing.T) (*store.Store, http.Handler) {
	t.Helper()
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "b1", Name: "节点B1", Group: "old", Provider: "p1", Region: "r1", Currency: "USD"})
	st.UpsertNode(&model.Node{ID: "b2", Name: "节点B2", Group: "old", Provider: "p1", Region: "r1", Currency: "USD"})
	srv := New(st, nil, nil, Config{AdminToken: "test-admin-token-1234567890"})
	return st, srv.Handler()
}

func batchPost(h http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/admin/nodes/batch", strings.NewReader(body))
	req.Header.Set("X-Admin-Token", "test-admin-token-1234567890")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestNodesBatchUpdate 批量改分组：一次请求改全部，不允许"改一半"。
func TestNodesBatchUpdate(t *testing.T) {
	st, h := batchEnv(t)
	w := batchPost(h, `{"ids":["b1","b2"],"action":"group","value":"prod"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	for _, id := range []string{"b1", "b2"} {
		if n, ok := st.Node(id); !ok || n.Group != "prod" {
			t.Errorf("%s 分组未更新: %+v", id, n)
		}
	}
}

// TestNodesBatchDelete 批量删除。
func TestNodesBatchDelete(t *testing.T) {
	st, h := batchEnv(t)
	w := batchPost(h, `{"ids":["b1","b2"],"action":"delete"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	if _, ok := st.Node("b1"); ok {
		t.Error("b1 应被删除")
	}
	if _, ok := st.Node("b2"); ok {
		t.Error("b2 应被删除")
	}
}

// TestNodesBatchValidatesExistence 任一 ID 不存在必须整体拒绝：
// 中途失败会留下"改了一半"的台账，且没有事务可回滚。
func TestNodesBatchValidatesExistence(t *testing.T) {
	st, h := batchEnv(t)
	w := batchPost(h, `{"ids":["b1","ghost"],"action":"group","value":"prod"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("含不存在节点应 400，实际 %d", w.Code)
	}
	if n, _ := st.Node("b1"); n.Group != "old" {
		t.Error("校验失败时不应改动任何节点（改一半不可回滚）")
	}
}

// TestNodesBatchCurrencyWhitelist 币种只允许 USD/CNY：
// 乱写字符串会让台账成本换算静默出错。
func TestNodesBatchCurrencyWhitelist(t *testing.T) {
	st, h := batchEnv(t)
	if w := batchPost(h, `{"ids":["b1"],"action":"currency","value":"EUR"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("非法币种应 400，实际 %d", w.Code)
	}
	if w := batchPost(h, `{"ids":["b1"],"action":"currency","value":"CNY"}`); w.Code != http.StatusOK {
		t.Fatalf("合法币种应 200，实际 %d", w.Code)
	}
	if n, _ := st.Node("b1"); n.Currency != "CNY" {
		t.Errorf("币种未更新: %+v", n)
	}
}

// TestNodesBatchRejectsNonAdmin 批量接口必须校验管理令牌。
func TestNodesBatchRejectsNonAdmin(t *testing.T) {
	_, h := batchEnv(t)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/nodes/batch",
		strings.NewReader(`{"ids":["b1"],"action":"delete"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("无令牌应 401，实际 %d", w.Code)
	}
}

// TestValidDate 到期日校验。
func TestValidDate(t *testing.T) {
	cases := map[string]bool{
		"2027-01-31": true, "2027-12-01": true, "2024-02-29": true,
		"2027-13-01": false, "2027-00-10": false, "2027-01-32": false,
		"2026-02-31": false, "2026-04-31": false, "2023-02-29": false,
		"2027/01/01": false, "20270101": false, "2027-1-1": false, "": false,
	}
	for in, want := range cases {
		if got := validDate(in); got != want {
			t.Errorf("validDate(%q) = %v，期望 %v", in, got, want)
		}
	}
}

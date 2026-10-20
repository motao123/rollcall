package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

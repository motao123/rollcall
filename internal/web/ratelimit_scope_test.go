package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAuthGuardScopesFailuresByRouteLevel 防止低权限请求替 admin 爆破「洗白」。
//
// 同一出口 IP 下，/metrics 或只读面板的正常轮询若清空全部失败计数，
// 攻击者每轮试 9 次 admin token、再等一次正常轮询即可无限重试。
func TestAuthGuardScopesFailuresByRouteLevel(t *testing.T) {
	s := &Server{limiter: newAuthFailureLimiter(), authUser: "admin"}

	levelOf := func(r *http.Request) authLevel {
		if r.URL.Path == "/metrics" {
			return authMetrics
		}
		return authAdmin
	}
	h := s.withAuthGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}), levelOf)

	req := func(path string) int {
		r := httptest.NewRequest("GET", path, nil)
		r.RemoteAddr = "203.0.113.9:12345"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}

	for i := 0; i < 10; i++ {
		req("/api/admin/nodes")
	}
	if code := req("/api/admin/nodes"); code != http.StatusTooManyRequests {
		t.Fatalf("连续失败后应 429，实际 %d", code)
	}

	if code := req("/metrics"); code != http.StatusOK {
		t.Fatalf("只读请求应正常，实际 %d", code)
	}
	if code := req("/api/admin/nodes"); code != http.StatusTooManyRequests {
		t.Fatalf("只读成功不应解除 admin 封禁，实际 %d", code)
	}
}

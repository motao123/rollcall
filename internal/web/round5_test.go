package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/store"
)

// TestNodeStateEmitsZeroForOtherStates 是回归测试。
//
// 原来只输出当前状态那一条（恒为 1）。节点一旦切换状态，旧序列是
// 「消失」而不是归 0，Prometheus 无法据此判断状态迁移。
func TestNodeStateEmitsZeroForOtherStates(t *testing.T) {
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "节点一"})
	st.AddMetrics("n1", model.HostInfo{}, model.Metrics{TS: time.Now().Unix(), CPUPct: 1})
	srv := New(st, nil, nil, Config{AdminToken: "test-admin-token-1234567890"})

	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("X-Admin-Token", "test-admin-token-1234567890")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, stt := range []string{model.NodeStateOnline, model.NodeStateStale, model.NodeStateOffline, model.NodeStateNoData} {
		if !strings.Contains(body, "state=\""+stt+"\"") {
			t.Fatalf("/metrics 缺少状态 %s 的序列", stt)
		}
	}
	if !strings.Contains(body, "state=\"online\"} 1") {
		t.Fatalf("在线节点应输出 1: %s", firstLines(body, 3))
	}
	if !strings.Contains(body, "state=\"offline\"} 0") {
		t.Fatalf("非当前状态应输出 0: %s", firstLines(body, 3))
	}
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, " | ")
}

// TestStatusRecorderUnwraps 防止中间件吞掉 http.ResponseController 的能力。
func TestStatusRecorderUnwraps(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: &nopWriter{}}
	if _, ok := rec.Unwrap().(*nopWriter); !ok {
		t.Fatalf("Unwrap 应返回原始 ResponseWriter，实际 %T", rec.Unwrap())
	}
}

type nopWriter struct{}

func (nopWriter) Header() http.Header         { return http.Header{} }
func (nopWriter) Write(b []byte) (int, error) { return len(b), nil }
func (nopWriter) WriteHeader(statusCode int)  {}

package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/motao/rollcall/internal/store"
)

func newHealthServer(t *testing.T) http.Handler {
	t.Helper()
	const tok = "health-test-admin-token-123456"
	return New(store.New(t.TempDir()), nil, nil, Config{AdminToken: tok}).Handler()
}

// TestAdminHealthRequiresAuth 确认实例状态不对外泄漏。
func TestAdminHealthRequiresAuth(t *testing.T) {
	h := newHealthServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/health", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("未认证访问实例健康应 401，实际 %d", w.Code)
	}
}

// TestAdminHealthReportsRatesAndPhases 覆盖卡片依赖的字段：
// 节点统计、上报速率（含"应有上报数"）、快照分段耗时。
func TestAdminHealthReportsRatesAndPhases(t *testing.T) {
	h := newHealthServer(t)
	const tok = "health-test-admin-token-123456"

	// 建一个节点并上报一次，让速率不为 0
	create := httptest.NewRequest(http.MethodPost, "/api/admin/nodes",
		bodyOf(`{"name":"h1","provider":"test"}`))
	create.Header.Set("Content-Type", "application/json")
	create.Header.Set("X-Admin-Token", tok)
	cw := httptest.NewRecorder()
	h.ServeHTTP(cw, create)
	if cw.Code != 201 {
		t.Fatalf("建节点失败: %d %s", cw.Code, cw.Body.String())
	}
	var node struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(cw.Body.Bytes(), &node); err != nil || node.Token == "" {
		t.Fatalf("建节点响应缺少令牌: %s", cw.Body.String())
	}

	rep := httptest.NewRequest(http.MethodPost, "/api/report",
		bodyOf(`{"nodeId":"`+node.ID+`","host":{"hostname":"h1"},"metrics":{"cpuPct":12.5,"memUsed":1,"memTotal":2}}`))
	rep.Header.Set("Content-Type", "application/json")
	rep.Header.Set("X-Node-Token", node.Token)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, rep)
	if rw.Code != 200 {
		t.Fatalf("上报失败: %d %s", rw.Code, rw.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/health", nil)
	req.Header.Set("X-Admin-Token", tok)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("健康接口应 200，实际 %d: %s", w.Code, w.Body.String())
	}

	var got struct {
		Version string `json:"version"`
		Up      int64  `json:"uptimeSeconds"`
		Mem     struct {
			HeapAllocBytes uint64 `json:"heapAllocBytes"`
		} `json:"memory"`
		Nodes struct {
			Total  int `json:"total"`
			Online int `json:"online"`
		} `json:"nodes"`
		Reports struct {
			Last60s   int64   `json:"last60s"`
			PerSecond float64 `json:"perSecond"`
			Expected  int     `json:"expected"`
		} `json:"reports"`
		Save struct {
			Count          int64   `json:"count"`
			Failures       int64   `json:"failures"`
			LastDurationMs float64 `json:"lastDurationMs"`
			MarshalMs      float64 `json:"marshalMs"`
			EncodeMs       float64 `json:"encodeMs"`
		} `json:"save"`
		Notify struct {
			Pending int `json:"pending"`
		} `json:"notify"`
		LimiterEntries int `json:"limiterEntries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}

	if got.Version == "" {
		t.Error("缺少版本号")
	}
	if got.Up < 0 {
		t.Error("运行时长异常")
	}
	if got.Nodes.Total != 1 {
		t.Errorf("节点总数应为 1，实际 %d", got.Nodes.Total)
	}
	if got.Reports.Last60s < 1 {
		t.Errorf("上报速率未统计到刚才的一次上报: %+v", got.Reports)
	}
	if got.Reports.Expected <= 0 {
		t.Errorf("应有上报数应按在线节点与推荐间隔计算: %+v", got.Reports)
	}
	if got.Mem.HeapAllocBytes == 0 {
		t.Error("堆内存未采集")
	}
	if got.LimiterEntries < 0 {
		t.Error("限速器条目数异常")
	}
}

// TestReportCounterWindow 确认滑窗会丢弃 60 秒之前的数据，
// 否则"上报速率"会变成永不下降的累计值，失去诊断意义。
func TestReportCounterWindow(t *testing.T) {
	var c reportCounter
	base := int64(1_700_000_000)
	for i := 0; i < 5; i++ {
		c.hit(base)
	}
	if got := c.last60s(base + 30); got != 5 {
		t.Fatalf("窗口内应统计到 5 次，实际 %d", got)
	}
	if got := c.last60s(base + 61); got != 0 {
		t.Fatalf("超过 60 秒应清零，实际 %d", got)
	}
	// 同一桶被新的一秒复用时，旧计数必须重置
	c.hit(base + 60) // 与 base 同一个 slot
	if got := c.last60s(base + 60); got != 1 {
		t.Fatalf("同一桶复用时应只剩新计数，实际 %d", got)
	}
}

func bodyOf(s string) io.Reader { return strings.NewReader(s) }

package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/store"
)

func newHealthServer(t *testing.T) http.Handler {
	t.Helper()
	const tok = "health-test-admin-token-123456"
	st := store.New(t.TempDir())
	// 公开页自 v1.15.2 起默认关闭；这些用例只关心健康与上报，
	// 先把公开页打开，避免被门槛挡在 404。
	enablePublicStatus(st)
	return New(st, nil, nil, Config{AdminToken: tok}).Handler()
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
	// 小幅 NTP 校正产生的"未来"桶仍在窗口内
	var c2 reportCounter
	c2.hit(base + 5)
	if got := c2.last60s(base); got != 1 {
		t.Fatalf("小幅时钟校正后应保留计数，实际 %d", got)
	}
	// 大幅时钟回拨：回拨前写入的远期桶不能一直计入
	var c3 reportCounter
	c3.hit(base + 3600)
	if got := c3.last60s(base); got != 0 {
		t.Fatalf("远期桶不应计入当前窗口，实际 %d", got)
	}
}

// TestUnauthenticatedReportsDoNotCount 回归：上报速率只统计通过鉴权的上报。
// 否则扫描器或配错令牌的 Agent 会让健康卡片误判"采集正常"。
func TestUnauthenticatedReportsDoNotCount(t *testing.T) {
	const tok = "health-test-admin-token-123456"
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "n1", Token: "good-token"})
	h := New(st, nil, nil, Config{AdminToken: tok}).Handler()

	post := func(nodeToken string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/report",
			bodyOf(`{"nodeId":"n1","host":{},"metrics":{}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Node-Token", nodeToken)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	reportCount := func() int64 {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/health", nil)
		req.Header.Set("X-Admin-Token", tok)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("健康接口应 200，实际 %d: %s", w.Code, w.Body.String())
		}
		var got struct {
			Reports struct {
				Last60s int64 `json:"last60s"`
			} `json:"reports"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("响应不是合法 JSON: %v", err)
		}
		return got.Reports.Last60s
	}

	if code := post("wrong-token"); code != http.StatusUnauthorized {
		t.Fatalf("错误令牌应 401，实际 %d", code)
	}
	if got := reportCount(); got != 0 {
		t.Fatalf("未认证上报不应计入速率，实际 %d", got)
	}
	if code := post("good-token"); code != http.StatusOK {
		t.Fatalf("有效上报应 200，实际 %d", code)
	}
	if got := reportCount(); got != 1 {
		t.Fatalf("有效上报应计入速率，实际 %d", got)
	}
}

// TestAdminHealthCountsNeverReportedAsNoData 回归：从未上报的节点
// 属于"待接入"，不能混入"离线"（离线表示曾经上报后失联）。
func TestAdminHealthCountsNeverReportedAsNoData(t *testing.T) {
	const tok = "health-test-admin-token-123456"
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "n1", Token: "t"})
	h := New(st, nil, nil, Config{AdminToken: tok}).Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/admin/health", nil)
	req.Header.Set("X-Admin-Token", tok)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("健康接口应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Nodes struct {
			Total   int `json:"total"`
			Online  int `json:"online"`
			Offline int `json:"offline"`
			NoData  int `json:"noData"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if got.Nodes.Total != 1 || got.Nodes.NoData != 1 || got.Nodes.Offline != 0 {
		t.Fatalf("从未上报的节点应计为 noData，实际 %+v", got.Nodes)
	}
}

func bodyOf(s string) io.Reader { return strings.NewReader(s) }

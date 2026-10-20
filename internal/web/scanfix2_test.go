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

// ---------- 限速器 ----------

// TestLimiterSucceedDoesNotUnblock 是回归：
//
// succeed 曾无条件删除整条状态，连 blockedUntil 一起清掉。攻击者手里只要
// 有一个有效凭证（或恰好命中一次不需要鉴权的成功请求），就能在封禁期间
// 把自己解封，封禁形同虚设。
func TestLimiterSucceedDoesNotUnblock(t *testing.T) {
	l := newAuthFailureLimiter()
	const ip = "203.0.113.7"
	for i := 0; i < l.maxFails; i++ {
		if ok, _ := l.allow(ip); !ok {
			t.Fatalf("第 %d 次应放行", i+1)
		}
		l.fail(ip)
	}
	if ok, _ := l.allow(ip); ok {
		t.Fatal("达到阈值后应处于封禁状态")
	}

	l.succeed(ip)

	if ok, _ := l.allow(ip); ok {
		t.Fatal("succeed 不应解除封禁（攻击者可用一次成功请求绕过限速）")
	}
}

// TestLimiterSweepKeepsInflight 是回归：
// sweepLocked 曾不看在途名额就删条目，随后的 fail/release 找不到条目会
// 重建一个干净状态，等于把已累计的失败次数清零。
func TestLimiterSweepKeepsInflight(t *testing.T) {
	l := newAuthFailureLimiter()
	now := time.Now()

	l.mu.Lock()
	l.state["198.51.100.9"] = &authFailState{windowStart: now.Add(-time.Hour), inflight: 1}
	l.state["198.51.100.10"] = &authFailState{windowStart: now.Add(-time.Hour)}
	l.lastSweep = now.Add(-2 * time.Minute)
	l.sweepLocked(now)
	_, kept := l.state["198.51.100.9"]
	_, gone := l.state["198.51.100.10"]
	l.mu.Unlock()

	if !kept {
		t.Error("有在途名额的条目被清扫删除")
	}
	if gone {
		t.Error("已过期且无在途名额的条目应被清扫")
	}
}

// ---------- 公开页 ----------

// TestStatusLatestOmitsMetricsForUnreportedNode 是回归：
// 从未上报过的节点 Latest 返回零值，节点详情接口会把 cpu 0 / memPct 0
// 下发出去，访客读到的是"机器空闲"，而真实情况是"没有数据"。
func TestStatusLatestOmitsMetricsForUnreportedNode(t *testing.T) {
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "未上报"})
	srv := New(st, nil, nil, Config{})
	h := srv.Handler()

	// 详情路由用公开页的哈希 ID（hid），不是内部节点 ID。
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var list struct {
		Nodes []struct {
			HID string `json:"hid"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Nodes) == 0 {
		t.Fatalf("解析公开列表失败: %v", err)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/status/node/"+list.Nodes[0].HID, nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("节点详情接口失败: %d %s", w.Code, w.Body.String())
	}
	// 详情里指标字段受 visibility 控制；未上报时 latest 必须是空对象
	// （注意响应里还有 "cpu":[] 这样的图表序列数组，那是另一回事）。
	var detail struct {
		Latest map[string]any `json:"latest"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatalf("解析详情失败: %v", err)
	}
	if len(detail.Latest) != 0 {
		t.Fatalf("未上报节点不应下发指标，实际: %v", detail.Latest)
	}
}

// ---------- 静态资源 ----------

// TestStaticConditionalOnlyForGET 是回归：
// 条件请求曾不限方法，POST/PUT 命中静态路径且 If-None-Match 匹配时返回 304，
// 调用方会把"没执行"当成"成功"。
func TestStaticConditionalOnlyForGET(t *testing.T) {
	srv := New(store.New(t.TempDir()), nil, nil, Config{})
	h := srv.Handler()

	// 先取一次拿 ETag
	req := httptest.NewRequest(http.MethodGet, "/app.js", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Skip("未启用静态资源（测试环境缺少 dist）")
	}

	req = httptest.NewRequest(http.MethodGet, "/app.js", nil)
	req.Header.Set("If-None-Match", etag)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotModified {
		t.Fatalf("GET 条件命中应 304，实际 %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/app.js", nil)
	req.Header.Set("If-None-Match", etag)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code == http.StatusNotModified {
		t.Fatal("POST 不应因 If-None-Match 命中返回 304")
	}
}

// ---------- 节点 ID ----------

// TestNodeByIDRejectsOversizedID 是回归：
// 路径剩余部分直接当 ID 用，长度与字符集都不限，会把任意载荷带进日志与响应。
func TestNodeByIDRejectsOversizedID(t *testing.T) {
	const tok = "id-guard-token-123456"
	srv := New(store.New(t.TempDir()), nil, nil, Config{AdminToken: tok})
	h := srv.Handler()

	long := strings.Repeat("a", 300)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/nodes/"+long, nil)
	req.Header.Set("X-Admin-Token", tok)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("超长 ID 应 400，实际 %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/admin/nodes/n1%20%3Cscript%3E", nil)
	req.Header.Set("X-Admin-Token", tok)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法字符 ID 应 400，实际 %d", w.Code)
	}
}

// ---------- 删除语义 ----------

// TestAddMetricsDoesNotResurrectDeletedNode 是回归：
// 删除与上报并发时，删除后的那次上报会重建 series/hosts/traffic，
// 让删掉的节点永远留在统计与导出里（它再也拿不到上报，无法自愈）。
func TestAddMetricsDoesNotResurrectDeletedNode(t *testing.T) {
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "旧节点"})
	ts := time.Now().Unix()
	st.AddMetrics("n1", model.HostInfo{Hostname: "old"}, model.Metrics{TS: ts, CPUPct: 10})

	if !st.DeleteNode("n1") {
		t.Fatal("删除节点失败")
	}
	// 在途上报（删除之后才落地）
	st.AddMetrics("n1", model.HostInfo{Hostname: "old"}, model.Metrics{TS: ts + 30, CPUPct: 99})

	if _, ok := st.Node("n1"); ok {
		t.Fatal("已删除的节点被在途上报复活")
	}
	if got := st.Series("n1", 0); len(got) != 0 {
		t.Fatalf("已删除节点不应保留指标序列，实际 %d 点", len(got))
	}

	// 重新创建同 ID 后必须能正常接收（墓碑要撤销）
	st.UpsertNode(&model.Node{ID: "n1", Name: "新节点"})
	st.AddMetrics("n1", model.HostInfo{Hostname: "new"}, model.Metrics{TS: ts + 60, CPUPct: 20})
	if got := st.Series("n1", 0); len(got) == 0 {
		t.Fatal("重建后的节点应能正常接收上报")
	}
}

// TestDeleteServiceSuppressesPendingNotify 是回归：
// 删除服务只关闭告警、不处理已排队的通知，会留下永不过期的 pending 记录
// （并可能继续投递已删除服务的告警）。
func TestDeleteServiceSuppressesPendingNotify(t *testing.T) {
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "节点"})
	st.UpsertService(&model.Service{ID: "s1", NodeID: "n1", Name: "Web", Type: "http", Target: "http://127.0.0.1", Enabled: true})
	rule := &model.AlertRule{ID: "r1", Name: "服务故障", Metric: "service_down", Op: "gt", Threshold: 1, Enabled: true}
	st.UpsertRule(rule)

	a := &model.Alert{
		ID: "a1", RuleID: "r1", RuleName: "服务故障", NodeID: "n1", NodeName: "节点",
		ServiceID: "s1", ServiceName: "Web", Severity: "crit", StartedAt: time.Now().Unix(),
	}
	if _, ok := st.OpenAlert(a); !ok {
		t.Fatal("创建告警失败")
	}
	job := &model.NotifyJob{
		ID: "j1", AlertID: "a1", RuleID: "r1", NodeID: "n1", Status: model.NotifyPending,
	}
	if !st.EnqueueNotifyJob(job) {
		t.Fatal("入队通知失败")
	}

	st.DeleteService("s1")

	var got *model.NotifyJob
	for _, j := range st.NotifyJobs(50) {
		if j.ID == "j1" {
			got = j
			break
		}
	}
	if got == nil {
		t.Fatal("通知任务不应被直接删除")
	}
	if got.Status == model.NotifyPending {
		t.Fatalf("删除服务后 pending 通知应被抑制，实际状态 %q", got.Status)
	}
}

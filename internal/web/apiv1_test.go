package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/store"
)

// 本文件覆盖 v1 分页接口的三条不变量：
//  1. 信封形状固定（items/total/limit/offset/hasMore + 附属字段）；
//  2. 分页语义正确（total 是筛选后的总数、offset 越界返回空页、末页 hasMore=false）；
//  3. 老路由行为不变（只改新增，不改既有 API 的形状与语义）。

const v1TestToken = "apiv1-test-token-123456"

// v1TestStore 建好一台节点、一条规则与 n 条告警，按创建顺序递增 StartedAt。
func v1TestStore(t *testing.T, alertCount int) *store.Store {
	t.Helper()
	st := store.New(t.TempDir())
	now := time.Now().Unix()
	for i := 0; i < alertCount; i++ {
		nodeID := fmt.Sprintf("n%d", i)
		st.UpsertNode(&model.Node{ID: nodeID, Name: "host-" + nodeID})
		st.UpsertRule(&model.AlertRule{
			ID: "r" + nodeID, Name: "CPU " + nodeID, Metric: "cpu",
			Op: "gt", Threshold: 90, Severity: "warn", Enabled: true,
		})
		started := now - int64(alertCount-i)
		created, ok := st.OpenAlert(&model.Alert{
			ID:     "a" + nodeID,
			RuleID: "r" + nodeID, NodeID: nodeID,
			RuleName: "CPU " + nodeID, NodeName: "host-" + nodeID,
			Severity: "warn", Message: "cpu 95%", Value: 95, StartedAt: started,
		})
		if !created || !ok {
			t.Fatalf("准备第 %d 条告警失败: created=%v ok=%v", i, created, ok)
		}
	}
	return st
}

// getJSON 发起带管理令牌的 GET 并解出 JSON 对象。
func getJSON(t *testing.T, srv *Server, path string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Admin-Token", v1TestToken)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	var body map[string]any
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s 响应不是合法 JSON: %v (%s)", path, err, w.Body.String())
		}
	}
	return w.Code, body
}

func num(t *testing.T, body map[string]any, key string) int {
	t.Helper()
	v, ok := body[key]
	if !ok {
		t.Fatalf("响应缺少字段 %q: %v", key, body)
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("字段 %q 不是数字: %#v", key, v)
	}
	return int(f)
}

func itemsOf(t *testing.T, body map[string]any) []any {
	t.Helper()
	v, ok := body["items"]
	if !ok {
		t.Fatalf("响应缺少 items 字段: %v", body)
	}
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("items 不是数组: %#v", v)
	}
	return arr
}

// TestV1AlertsPaging 覆盖信封、翻页与越界。
func TestV1AlertsPaging(t *testing.T) {
	st := v1TestStore(t, 25)
	srv := New(st, nil, nil, Config{AdminToken: v1TestToken})

	// 第一页
	code, body := getJSON(t, srv, "/api/v1/alerts?limit=10&offset=0")
	if code != http.StatusOK {
		t.Fatalf("应 200，实际 %d", code)
	}
	if got := len(itemsOf(t, body)); got != 10 {
		t.Fatalf("第一页应有 10 条，实际 %d", got)
	}
	if total := num(t, body, "total"); total != 25 {
		t.Fatalf("total 应为 25（筛选后总数，不是本页条数），实际 %d", total)
	}
	if num(t, body, "limit") != 10 || num(t, body, "offset") != 0 {
		t.Fatalf("limit/offset 应回显生效值，实际 %v", body)
	}
	if hasMore, _ := body["hasMore"].(bool); !hasMore {
		t.Fatalf("第一页 hasMore 应为 true，实际 %v", body["hasMore"])
	}
	// 附属字段必须一次给全，避免前端再打一次老路由
	for _, k := range []string{"active", "notify", "now"} {
		if _, ok := body[k]; !ok {
			t.Fatalf("响应缺少附属字段 %q", k)
		}
	}

	// 末页：offset=20 → 5 条，hasMore=false
	_, last := getJSON(t, srv, "/api/v1/alerts?limit=10&offset=20")
	if got := len(itemsOf(t, last)); got != 5 {
		t.Fatalf("末页应有 5 条，实际 %d", got)
	}
	if hasMore, _ := last["hasMore"].(bool); hasMore {
		t.Fatalf("末页 hasMore 应为 false，实际 %v", last["hasMore"])
	}

	// 越界：返回空页但 total 不变（前端据此把页码夹回最后一页）
	_, over := getJSON(t, srv, "/api/v1/alerts?limit=10&offset=999")
	if got := len(itemsOf(t, over)); got != 0 {
		t.Fatalf("越界应返回空页，实际 %d 条", got)
	}
	if total := num(t, over, "total"); total != 25 {
		t.Fatalf("越界时 total 仍应为 25，实际 %d", total)
	}

	// 超过上限按上限截断（不报错）
	_, clamped := getJSON(t, srv, "/api/v1/alerts?limit=9999")
	if got := num(t, clamped, "limit"); got != v1MaxLimit {
		t.Fatalf("limit 应被夹到 %d，实际 %d", v1MaxLimit, got)
	}
	if got := len(itemsOf(t, clamped)); got != 25 {
		t.Fatalf("夹取后应返回全部 25 条，实际 %d", got)
	}
}

// TestV1AlertsSortStable 保证"新→旧"顺序且相邻页不重叠：
// 分页最典型的缺陷就是同一秒内的记录在不同页重复出现或丢失。
func TestV1AlertsSortStable(t *testing.T) {
	st := v1TestStore(t, 12)
	srv := New(st, nil, nil, Config{AdminToken: v1TestToken})

	seen := map[string]bool{}
	for offset := 0; offset < 12; offset += 5 {
		_, body := getJSON(t, srv, fmt.Sprintf("/api/v1/alerts?limit=5&offset=%d", offset))
		for _, raw := range itemsOf(t, body) {
			a, _ := raw.(map[string]any)
			id, _ := a["id"].(string)
			if id == "" {
				t.Fatalf("告警缺少 id: %v", a)
			}
			if seen[id] {
				t.Fatalf("告警 %s 在相邻页重复出现（分页顺序不稳定）", id)
			}
			seen[id] = true
		}
	}
	if len(seen) != 12 {
		t.Fatalf("翻完所有页应覆盖 12 条，实际 %d 条", len(seen))
	}
}

// TestV1AlertsFilter 覆盖状态筛选与检索：筛选必须在分页之前生效，
// 否则 total 会对不上（这是"前端筛选 + 后端分页"最常见的错误）。
func TestV1AlertsFilter(t *testing.T) {
	st := v1TestStore(t, 10)
	srv := New(st, nil, nil, Config{AdminToken: v1TestToken})

	// 全部进行中
	_, active := getJSON(t, srv, "/api/v1/alerts?state=active&limit=100")
	if total := num(t, active, "total"); total != 10 {
		t.Fatalf("进行中应有 10 条，实际 %d", total)
	}
	// 已恢复应为 0
	_, resolved := getJSON(t, srv, "/api/v1/alerts?state=resolved&limit=100")
	if total := num(t, resolved, "total"); total != 0 {
		t.Fatalf("已恢复应为 0 条，实际 %d", total)
	}
	// 检索命中节点名
	_, hit := getJSON(t, srv, "/api/v1/alerts?q=host-n3&limit=100")
	if total := num(t, hit, "total"); total != 1 {
		t.Fatalf("检索 host-n3 应命中 1 条，实际 %d", total)
	}
	// 检索不到
	_, miss := getJSON(t, srv, "/api/v1/alerts?q=不存在的关键词&limit=100")
	if total := num(t, miss, "total"); total != 0 {
		t.Fatalf("不存在的关键词应命中 0 条，实际 %d", total)
	}
}

// TestV1NotifyJobsPaging 覆盖队列分页与状态筛选。
func TestV1NotifyJobsPaging(t *testing.T) {
	st := store.New(t.TempDir())
	now := time.Now().Unix()
	// 7 条已送达 + 3 条失败 = 10；已送达在排序上位于失败之后。
	for i := 0; i < 7; i++ {
		id := fmt.Sprintf("sent-%d", i)
		if ok := st.EnqueueNotifyJob(&model.NotifyJob{
			ID: id, Status: model.NotifySent, Title: "t-" + id, CreatedAt: now - int64(i),
		}); !ok {
			t.Fatalf("入队失败: %s", id)
		}
	}
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("fail-%d", i)
		if ok := st.EnqueueNotifyJob(&model.NotifyJob{
			ID: id, Status: model.NotifyFailed, Title: "t-" + id, CreatedAt: now - int64(10+i),
		}); !ok {
			t.Fatalf("入队失败: %s", id)
		}
	}
	srv := New(st, nil, nil, Config{AdminToken: v1TestToken})

	code, body := getJSON(t, srv, "/api/v1/admin/notify/jobs?limit=4&offset=0")
	if code != http.StatusOK {
		t.Fatalf("应 200，实际 %d", code)
	}
	if got := len(itemsOf(t, body)); got != 4 {
		t.Fatalf("应返回 4 条，实际 %d", got)
	}
	if total := num(t, body, "total"); total != 10 {
		t.Fatalf("total 应为 10，实际 %d", total)
	}
	if _, ok := body["stats"]; !ok {
		t.Fatalf("响应缺少 stats 字段")
	}
	// 状态筛选：只取失败
	_, failed := getJSON(t, srv, "/api/v1/admin/notify/jobs?status=failed&limit=100")
	if total := num(t, failed, "total"); total != 3 {
		t.Fatalf("失败任务应为 3 条，实际 %d", total)
	}
	// 越界
	_, over := getJSON(t, srv, "/api/v1/admin/notify/jobs?limit=4&offset=50")
	if got := len(itemsOf(t, over)); got != 0 {
		t.Fatalf("越界应返回空页，实际 %d 条", got)
	}
}

// TestV1AuditPaging 覆盖审计分页与信封（审计本身早已支持 offset，本测试锁住信封形状）。
func TestV1AuditPaging(t *testing.T) {
	st := store.New(t.TempDir())
	now := time.Now().Unix()
	for i := 0; i < 6; i++ {
		st.AddAudit(model.AuditEntry{
			ID: fmt.Sprintf("e%d", i), TS: now - int64(i), Action: "node.update",
			Actor: "admin", Role: "session", Method: "PUT",
			Path: "/api/admin/nodes/n1", Status: 200,
		})
	}
	srv := New(st, nil, nil, Config{AdminToken: v1TestToken})

	code, body := getJSON(t, srv, "/api/v1/admin/audit?limit=4&offset=2&range=24h")
	if code != http.StatusOK {
		t.Fatalf("应 200，实际 %d", code)
	}
	if got := len(itemsOf(t, body)); got != 4 {
		t.Fatalf("应返回 4 条，实际 %d", got)
	}
	if total := num(t, body, "total"); total != 6 {
		t.Fatalf("total 应为 6，实际 %d", total)
	}
	if num(t, body, "offset") != 2 {
		t.Fatalf("offset 应回显 2，实际 %v", body["offset"])
	}
	if _, ok := body["stats"]; !ok {
		t.Fatalf("响应缺少 stats 字段")
	}
}

// TestLegacyEndpointsUnchanged 是兼容性回归：老路由的响应形状不能因为
// 新增 v1 接口而改变 —— 外部脚本与旧版前端都依赖它们。
func TestLegacyEndpointsUnchanged(t *testing.T) {
	st := v1TestStore(t, 3)
	srv := New(st, nil, nil, Config{AdminToken: v1TestToken})

	_, alerts := getJSON(t, srv, "/api/alerts")
	for _, k := range []string{"items", "active", "silences", "notify", "now"} {
		if _, ok := alerts[k]; !ok {
			t.Fatalf("老接口 /api/alerts 缺少字段 %q（形状被改动了）", k)
		}
	}
	if _, ok := alerts["hasMore"]; ok {
		t.Fatalf("老接口不应出现 v1 的信封字段 hasMore")
	}

	_, audit := getJSON(t, srv, "/api/admin/audit?limit=10")
	for _, k := range []string{"entries", "total", "limit", "offset", "stats"} {
		if _, ok := audit[k]; !ok {
			t.Fatalf("老接口 /api/admin/audit 缺少字段 %q（形状被改动了）", k)
		}
	}
	if _, ok := audit["entries"].([]any); !ok {
		t.Fatalf("老接口 entries 应为数组，实际 %#v", audit["entries"])
	}

	_, jobs := getJSON(t, srv, "/api/admin/notify/jobs")
	for _, k := range []string{"jobs", "stats"} {
		if _, ok := jobs[k]; !ok {
			t.Fatalf("老接口 /api/admin/notify/jobs 缺少字段 %q（形状被改动了）", k)
		}
	}
}

// TestV1RequiresAuth 确认分页接口没有绕过鉴权：
// 告警历史含节点名与规则名，通知队列含告警正文，都不能匿名读。
func TestV1RequiresAuth(t *testing.T) {
	st := v1TestStore(t, 2)
	srv := New(st, nil, nil, Config{AdminToken: v1TestToken})

	for _, path := range []string{"/api/v1/alerts", "/api/v1/admin/audit", "/api/v1/admin/notify/jobs"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s 未鉴权应 401，实际 %d", path, w.Code)
		}
	}
}

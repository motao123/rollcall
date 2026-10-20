package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/motao/rollcall/internal/model"
	"github.com/motao/rollcall/internal/store"
)

// statusTestEnv 起一个带单个节点的服务端并返回 Handler。
func statusTestEnv(t *testing.T) (*store.Store, http.Handler) {
	t.Helper()
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "节点一", Token: "tok", Group: "prod",
		Provider: "某服务商", Region: "香港"})
	st.UpsertNode(&model.Node{ID: "n2", Name: "节点二", Token: "tok2", Group: "prod"})
	srv := New(st, nil, nil, Config{AdminToken: "test-admin-token-1234567890"})
	return st, srv.Handler()
}

// detailURL 返回节点的公开详情地址。
// ★ 公开接口只认「公开句柄」，不认内部节点 ID（见 store.PublicHandle）：
// 测试必须走真实入口，否则测的是后门而不是线上行为。
func detailURL(st *store.Store, nodeID string) string {
	return "/api/status/node/" + st.PublicHandle(nodeID)
}

// report 把一份主机/指标快照灌进节点 n1。
func statusReport(h http.Handler, host, metrics string) {
	body := `{"nodeId":"n1","host":` + host + `,"metrics":` + metrics + `}`
	req := httptest.NewRequest(http.MethodPost, "/api/report", strings.NewReader(body))
	req.Header.Set("X-Node-Token", "tok")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		panic("report failed: " + w.Body.String())
	}
}

// TestStatusDetailHidesDisabledFields 是公开接口的安全回归：
// 可见性关闭的字段绝不能出现在 /api/status/node/* 的响应里。
// 这里故意把主机名/内核/进程数等设成"看起来有值"，关掉开关后逐个断言缺失。
func TestStatusDetailHidesDisabledFields(t *testing.T) {
	st, h := statusTestEnv(t)
	statusReport(h,
		`{"hostname":"secret-host","os":"Debian 12","kernel":"6.1.0-secret","arch":"x86_64","cpuModel":"Secret CPU","cores":8,"virt":"KVM"}`,
		`{"cpuPct":10,"load1":0.5,"load5":0.4,"load15":0.3,"uptime":3600,
		  "memUsed":1000,"memTotal":4000,"diskUsed":1000,"diskTotal":4000,
		  "swapUsed":10,"swapTotal":100,"netRx":1000,"netTx":2000,"procs":42,"tcpConn":7}`)

	cfg := model.DefaultStatusPage()
	cfg.Visibility.Hostname = false
	cfg.Visibility.OS = false
	cfg.Visibility.Kernel = false
	cfg.Visibility.Arch = false
	cfg.Visibility.CPUModel = false
	cfg.Visibility.Cores = false
	cfg.Visibility.Virt = false
	cfg.Visibility.Procs = false
	cfg.Visibility.TCPConn = false
	cfg.Visibility.Swap = false
	cfg.Visibility.Load = false
	st.SetStatusPage(cfg)

	req := httptest.NewRequest(http.MethodGet, detailURL(st, "n1"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("详情接口应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	host, _ := out["host"].(map[string]any)
	if host == nil {
		t.Fatalf("host 应为对象: %s", w.Body.String())
	}
	for _, k := range []string{"hostname", "os", "kernel", "arch", "cpuModel", "cores", "virt"} {
		if _, ok := host[k]; ok {
			t.Errorf("可见性关闭后 host.%s 仍被下发（泄露指纹信息）", k)
		}
	}
	latest, _ := out["latest"].(map[string]any)
	if latest == nil {
		t.Fatalf("latest 应为对象: %s", w.Body.String())
	}
	for _, k := range []string{"procs", "tcpConn", "swapUsed", "swapTotal", "cores", "load1", "load5", "load15"} {
		if _, ok := latest[k]; ok {
			t.Errorf("可见性关闭后 latest.%s 仍被下发", k)
		}
	}
	// 指标默认开放，应存在
	for _, k := range []string{"cpu", "memPct", "diskPct"} {
		if _, ok := latest[k]; !ok {
			t.Errorf("默认开放的 latest.%s 不应缺失", k)
		}
	}
}

// TestStatusDetailShowsEnabledFields 确认开关打开后字段确实出现，
// 与上一个测试构成双向验证：关了没有、开了才有。
func TestStatusDetailShowsEnabledFields(t *testing.T) {
	st, h := statusTestEnv(t)
	statusReport(h,
		`{"hostname":"web-01","os":"Debian 12","kernel":"6.1.0","arch":"x86_64","cpuModel":"X","cores":4,"virt":"KVM"}`,
		`{"cpuPct":10,"procs":7,"uptime":60}`)

	cfg := model.DefaultStatusPage()
	cfg.Visibility.Hostname = true
	cfg.Visibility.OS = true
	cfg.Visibility.Procs = true
	st.SetStatusPage(cfg)

	req := httptest.NewRequest(http.MethodGet, detailURL(st, "n1"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var out struct {
		Host   map[string]any `json:"host"`
		Latest map[string]any `json:"latest"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	for _, k := range []string{"hostname", "os"} {
		if _, ok := out.Host[k]; !ok {
			t.Errorf("开启后 host.%s 应下发", k)
		}
	}
	if _, ok := out.Latest["procs"]; !ok {
		t.Error("开启后 latest.procs 应下发")
	}
	// 仍未开启的字段继续隐藏
	if _, ok := out.Host["kernel"]; ok {
		t.Error("kernel 未开启却仍被下发")
	}
	_ = st
}

// TestStatusDetailRangeTiers 确认 7d/30d 走聚合层、24h 走原始层。
// 图表换区间不能悄悄退回 24h，否则用户以为在看一周数据，实际只有一天。
func TestStatusDetailRangeTiers(t *testing.T) {
	st, h := statusTestEnv(t)
	now := time.Now().Unix()
	for i := 0; i < 4; i++ {
		ts := now - int64(6-2*i)*86400
		st.AddMetrics("n1", model.HostInfo{}, model.Metrics{
			TS: ts, CPUPct: float64(10 + i),
			MemUsed: 100, MemTotal: 400, DiskUsed: 200, DiskTotal: 400,
			NetRx: uint64(i) * 1024, NetTx: uint64(i) * 2048,
		})
	}

	type seriesOut struct {
		CPU []model.Point `json:"cpu"`
	}
	type detail struct {
		Range  string    `json:"range"`
		Series seriesOut `json:"series"`
	}
	get := func(q string) detail {
		req := httptest.NewRequest(http.MethodGet, detailURL(st, "n1")+q, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s 应 200，实际 %d: %s", q, w.Code, w.Body.String())
		}
		var out detail
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s 响应不是合法 JSON: %v", q, err)
		}
		return out
	}

	for _, tc := range []struct {
		query string
		key   string
		want  int
	}{
		{"", "24h", 1}, // 只有最后 24h 内的原始点
		{"?range=7d", "7d", 4},
		{"?range=30d", "30d", 4},
		{"?range=bogus", "24h", 1},
	} {
		out := get(tc.query)
		if out.Range != tc.key {
			t.Errorf("%s 的 range=%q，期望 %q", tc.query, out.Range, tc.key)
		}
		if len(out.Series.CPU) != tc.want {
			t.Errorf("%s 的 CPU 点数=%d，期望 %d", tc.query, len(out.Series.CPU), tc.want)
		}
	}
}

// TestServiceLatencyOnlyCountsSuccess 是延迟曲线的语义回归：
// 失败探测的 latency 是 0，绝不能混入曲线（会显示成"延迟极低"）。
func TestServiceLatencyOnlyCountsSuccess(t *testing.T) {
	st, h := statusTestEnv(t)
	now := time.Now().Unix()
	st.UpsertService(&model.Service{
		ID: "svc1", NodeID: "n1", Name: "站点", Type: "http",
		Target: "http://127.0.0.1/healthz", Enabled: true,
	})
	st.AddServiceResult(model.ServiceResult{ServiceID: "svc1", TS: now - 120, OK: true, LatencyMs: 40})
	st.AddServiceResult(model.ServiceResult{ServiceID: "svc1", TS: now - 60, OK: false, LatencyMs: 0})

	req := httptest.NewRequest(http.MethodGet, detailURL(st, "n1"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var detail struct {
		Services []struct {
			Name       string        `json:"name"`
			Has        bool          `json:"has"`
			OK         bool          `json:"ok"`
			FailRate   float64       `json:"failRate"`
			Latency    []model.Point `json:"latency"`
			AvgLatency float64       `json:"avgLatency"`
			Samples    int           `json:"samples"`
		} `json:"services"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if len(detail.Services) != 1 {
		t.Fatalf("应返回 1 个服务，实际 %d", len(detail.Services))
	}
	sv := detail.Services[0]
	if !sv.Has || sv.OK {
		t.Errorf("最近一次探测应标记为失败: has=%v ok=%v", sv.Has, sv.OK)
	}
	if sv.FailRate != 50 {
		t.Errorf("失败率应为 50，实际 %v", sv.FailRate)
	}
	if len(sv.Latency) != 1 || sv.Latency[0].V != 40 {
		t.Errorf("延迟曲线应只含成功样本 40ms，实际 %#v", sv.Latency)
	}
	if sv.AvgLatency != 40 || sv.Samples != 1 {
		t.Errorf("平均延迟/样本数错误: avg=%v samples=%d", sv.AvgLatency, sv.Samples)
	}

	// 列表接口的迷你曲线遵循同一规则。
	req = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var list struct {
		Services []struct {
			FailRate     float64       `json:"failRate"`
			LatencySpark []model.Point `json:"latencySpark"`
		} `json:"services"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if len(list.Services) != 1 || len(list.Services[0].LatencySpark) != 1 ||
		list.Services[0].LatencySpark[0].V != 40 || list.Services[0].FailRate != 50 {
		t.Errorf("列表服务的延迟/失败率不正确: %#v", list.Services)
	}
}

// TestStatusDetailRespectsBlockSwitches 区块关闭后详情接口不再下发对应数据。
func TestStatusDetailRespectsBlockSwitches(t *testing.T) {
	st, h := statusTestEnv(t)
	cfg := model.DefaultStatusPage()
	cfg.Blocks.Services = false
	cfg.Blocks.History = false
	st.SetStatusPage(cfg)

	req := httptest.NewRequest(http.MethodGet, detailURL(st, "n1"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("详情应 200，实际 %d", w.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	for _, k := range []string{"services", "daily"} {
		if _, ok := out[k]; ok {
			t.Errorf("区块关闭后 %s 仍被下发", k)
		}
	}
}

// TestStatusListVisibilityGatesMetadata 确认列表里的新增字段也逐项受开关控制。
func TestStatusListVisibilityGatesMetadata(t *testing.T) {
	st, h := statusTestEnv(t)
	now := time.Now().Unix()
	st.AddMetrics("n1", model.HostInfo{
		Hostname: "web-01", OS: "Debian 12", Kernel: "6.1", Arch: "x86_64",
		CPUModel: "X", Cores: 8, Virt: "KVM",
	}, model.Metrics{
		TS: now, CPUPct: 12, MemUsed: 512, MemTotal: 1024, DiskUsed: 256, DiskTotal: 1024,
		NetRx: 4096, NetTx: 2048, Load1: 0.004, Uptime: 3600, Procs: 9, TCPConn: 3,
		SwapUsed: 1, SwapTotal: 2,
	})

	findNode := func(body []byte) map[string]any {
		var out struct {
			Nodes      []map[string]any `json:"nodes"`
			Visibility map[string]bool  `json:"visibility"`
			Blocks     map[string]bool  `json:"blocks"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("bad json: %v", err)
		}
		if out.Visibility == nil || out.Blocks == nil {
			t.Fatalf("响应应包含 visibility/blocks: %s", body)
		}
		for _, n := range out.Nodes {
			if n["name"] == "节点一" {
				return n
			}
		}
		t.Fatalf("响应中缺少节点一: %s", body)
		return nil
	}

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	n := findNode(w.Body.Bytes())
	for _, k := range []string{"os", "arch", "cores", "virt", "region", "provider", "hostname", "procs", "tcpConn", "swapUsed"} {
		if _, ok := n[k]; ok {
			t.Errorf("默认配置下列表泄露 %s", k)
		}
	}
	// 负载可见时，真实读数恰好为 0.00 也必须下发（不能用"零值省略"）。
	if v, ok := n["load1"]; !ok || v != float64(0) {
		t.Errorf("负载为 0 时应下发 load1=0，实际 %v（存在=%v）", v, ok)
	}

	cfg := model.DefaultStatusPage()
	cfg.Visibility.OS = true
	cfg.Visibility.Arch = true
	cfg.Visibility.Cores = true
	cfg.Visibility.Virt = true
	cfg.Visibility.Region = true
	cfg.Visibility.Provider = true
	cfg.Visibility.Procs = true
	cfg.Visibility.TCPConn = true
	cfg.Visibility.Swap = true
	st.SetStatusPage(cfg)

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	n = findNode(w.Body.Bytes())
	for _, k := range []string{"os", "arch", "cores", "virt", "region", "provider", "procs", "tcpConn", "swapUsed"} {
		if _, ok := n[k]; !ok {
			t.Errorf("开启后列表缺少 %s", k)
		}
	}
}

// TestStatusNodeDetailDisabled404 关闭"节点详情"区块后，
// 详情接口必须 404 而不是返回裁剪后的数据 —— 区块开关是入口开关。
func TestStatusNodeDetailDisabled404(t *testing.T) {
	st, h := statusTestEnv(t)
	cfg := model.DefaultStatusPage()
	cfg.Blocks.NodeDetail = false
	st.SetStatusPage(cfg)
	req := httptest.NewRequest(http.MethodGet, detailURL(st, "n1"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("详情关闭后应 404，实际 %d", w.Code)
	}
}

// TestStatusSummaryCounts 汇总条的人数与可用率均值。
// no_data 节点不能被计入可用率均值 —— 那会把整体可用率拉低制造假警报。
func TestStatusSummaryCounts(t *testing.T) {
	_, h := statusTestEnv(t)
	statusReport(h, `{"hostname":"a"}`, `{"cpuPct":10,"memUsed":1000,"memTotal":4000,"diskUsed":100,"diskTotal":400}`)
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var out struct {
		Summary statusSummary `json:"summary"`
		Nodes   []struct {
			ID       string  `json:"id"`
			State    string  `json:"state"`
			Uptime24 float64 `json:"uptime24"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if out.Summary.Total != 2 {
		t.Errorf("total = %d，期望 2", out.Summary.Total)
	}
	if out.Summary.Online != 1 {
		t.Errorf("online = %d，期望 1（另一台无数据）", out.Summary.Online)
	}
	if out.Summary.NoData != 1 {
		t.Errorf("noData = %d，期望 1", out.Summary.NoData)
	}
	// 只有 1 台有数据，均值就是它自己的 100%
	if out.Summary.AvgUptime24 != 100 {
		t.Errorf("avgUptime24 = %v，期望 100（no_data 不应计入均值）", out.Summary.AvgUptime24)
	}
}

// TestNodeDailyUptimeDayBucketing 校验可用率日柱按 UTC 切日、
// 无数据的天必须 Has=false（否则新节点会凭空多出一排红色）。
func TestNodeDailyUptimeDayBucketing(t *testing.T) {
	st, _ := statusTestEnv(t)
	srv := New(st, nil, nil, Config{AdminToken: "test-admin-token-1234567890"})

	now := time.Now().Unix()
	days := srv.nodeDailyUptime("n1", 7, now)
	if len(days) != 7 {
		t.Fatalf("应返回 7 天，实际 %d", len(days))
	}
	for _, d := range days {
		if d.Has {
			t.Errorf("节点从未上报，%d 不应有数据", d.TS)
		}
		if d.Up != 0 {
			t.Errorf("无数据天可用率应为 0，实际 %v", d.Up)
		}
	}
	// 起点必须落在 UTC 日界上
	if days[0].TS%86400 != 0 {
		t.Errorf("日柱起点 %d 未按 UTC 日对齐", days[0].TS)
	}
}

// TestPublicNodeHandleIsOpaque 是「公开页不含节点 ID」承诺的回归。
//
// 背景：详情页上线时曾直接把内部节点 ID 下发到 /api/status，
// README 的脱敏原则与 TestPublicStatusIsDesensitized 都明确禁止，
// 而 CI 因此变红。这里把"列表只能给句柄、详情只认句柄"钉死：
//  1. 列表里不出现内部 ID（连字符串形式都不能有）；
//  2. 用内部 ID 访问详情必须 404（否则句柄只是装饰）；
//  3. 句柄跨重启稳定（否则书签/分享链接会随机失效）。
func TestPublicNodeHandleIsOpaque(t *testing.T) {
	st, h := statusTestEnv(t)
	statusReport(h, `{}`, `{"cpuPct":10}`)

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body := w.Body.String()
	for _, leak := range []string{`"id"`, `"n1"`, `"n2"`} {
		if strings.Contains(body, leak) {
			t.Errorf("公开列表下发了内部标识 %s: %s", leak, body)
		}
	}

	var list struct {
		Nodes []struct {
			HID  string `json:"hid"`
			Name string `json:"name"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(list.Nodes) != 2 {
		t.Fatalf("应下发 2 个节点，实际 %d", len(list.Nodes))
	}
	h1 := st.PublicHandle("n1")
	found := false
	for _, n := range list.Nodes {
		if n.HID == "" {
			t.Errorf("节点 %q 缺少公开句柄", n.Name)
		}
		if n.HID == h1 {
			found = true
		}
	}
	if !found {
		t.Errorf("列表里找不到 n1 的句柄 %s", h1)
	}

	// 内部 ID 必须被拒绝：否则拿到 ID 就能绕开句柄直接看详情。
	req = httptest.NewRequest(http.MethodGet, "/api/status/node/n1", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("内部 ID 仍能访问详情（%d）：%s", w.Code, w.Body.String())
	}

	// 句柄必须真的能打开详情。
	req = httptest.NewRequest(http.MethodGet, detailURL(st, "n1"), nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("句柄访问详情应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"n1"`) {
		t.Errorf("详情仍下发内部 ID: %s", w.Body.String())
	}

	// 句柄跨重启稳定：盐值必须随快照落盘。
	dir := t.TempDir()
	a := store.New(dir)
	a.UpsertNode(&model.Node{ID: "n1", Name: "节点一", Token: "tok"})
	before := a.PublicHandle("n1")
	if err := a.Save(); err != nil {
		t.Fatalf("保存快照失败: %v", err)
	}
	b := store.New(dir)
	if after := b.PublicHandle("n1"); after != before {
		t.Errorf("重启后句柄变了（公开链接会全部失效）：%s → %s", before, after)
	}
}

// TestStatusPageSectionOrderRoundTrip 覆盖区块排序的完整链路：
// 保存顺序 → 公开页 payload 按该顺序下发 → 非法键被丢弃、缺失键补齐。
func TestStatusPageSectionOrderRoundTrip(t *testing.T) {
	st := store.New(t.TempDir())
	srv := New(st, nil, nil, Config{AdminToken: "sections-test-token-123456"})
	h := srv.Handler()

	// 1) 保存一个自定义顺序：把可用率（history）提到最前
	body := `{"sections":["history","nodes","services","map","incidents","activity"]}`
	req := httptest.NewRequest(http.MethodPut, "/api/admin/status-page", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin-Token", "sections-test-token-123456")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("保存顺序失败: %d %s", w.Code, w.Body.String())
	}

	// 2) 公开页必须按保存的顺序下发
	req = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("公开页接口失败: %d", w.Code)
	}
	var payload struct {
		Sections []string `json:"sections"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析 payload 失败: %v", err)
	}
	want := []string{"history", "nodes", "services", "map", "incidents", "activity"}
	if !reflect.DeepEqual(payload.Sections, want) {
		t.Fatalf("顺序未生效\n got: %v\nwant: %v", payload.Sections, want)
	}

	// 3) 非法/重复键被丢弃，缺失键按默认顺序补齐
	body = `{"sections":["activity","nope","activity"]}`
	req = httptest.NewRequest(http.MethodPut, "/api/admin/status-page", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin-Token", "sections-test-token-123456")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("保存异常顺序失败: %d %s", w.Code, w.Body.String())
	}
	got := st.StatusPage().Sections
	if len(got) != len(model.StatusSectionKeys) {
		t.Fatalf("区块数量应为 %d，实际 %d: %v", len(model.StatusSectionKeys), len(got), got)
	}
	if got[0] != "activity" {
		t.Errorf("合法项应保持在前: %v", got)
	}
	for _, k := range got {
		if k == "nope" {
			t.Errorf("非法键不应保留: %v", got)
		}
	}
	seen := map[string]bool{}
	for _, k := range got {
		if seen[k] {
			t.Errorf("出现重复键 %q: %v", k, got)
		}
		seen[k] = true
	}
}

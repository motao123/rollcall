package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	st.SetStatusPage(cfg)

	req := httptest.NewRequest(http.MethodGet, "/api/status/node/n1", nil)
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
	for _, k := range []string{"procs", "tcpConn", "swapUsed", "swapTotal", "cores"} {
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

	req := httptest.NewRequest(http.MethodGet, "/api/status/node/n1", nil)
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

// TestStatusNodeDetailDisabled404 关闭"节点详情"区块后，
// 详情接口必须 404 而不是返回裁剪后的数据 —— 区块开关是入口开关。
func TestStatusNodeDetailDisabled404(t *testing.T) {
	st, h := statusTestEnv(t)
	cfg := model.DefaultStatusPage()
	cfg.Blocks.NodeDetail = false
	st.SetStatusPage(cfg)
	req := httptest.NewRequest(http.MethodGet, "/api/status/node/n1", nil)
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

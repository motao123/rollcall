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

func TestProbeTargetHost(t *testing.T) {
	cases := []struct {
		typ, target, want string
	}{
		{"https", "https://Example.COM/health", "example.com"},
		{"https", "https://example.com:8443/x", "example.com"},
		{"http", "http://example.com", "example.com"},
		{"http", "example.com/health", "example.com"},
		{"tcp", "example.com:443", "example.com"},
		{"tcp", "[2001:DB8::1]:443", "2001:db8::1"},
		{"tcp", "2001:db8::1", "2001:db8::1"},
		{"dns", "Example.com@8.8.8.8", "example.com"},
		{"dns", "example.com.", "example.com"},
		{"dns", "example.com@1.1.1.1:53", "example.com"},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := probeTargetHost(c.typ, c.target); got != c.want {
			t.Errorf("probeTargetHost(%q, %q) = %q, want %q", c.typ, c.target, got, c.want)
		}
	}
}

func TestServiceCompareAPI(t *testing.T) {
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "东京"})
	st.UpsertNode(&model.Node{ID: "n2", Name: "香港"})

	add := func(id, node, name, typ, target string, enabled bool) {
		st.UpsertService(&model.Service{
			ID: id, NodeID: node, Name: name, Type: typ, Target: target,
			Interval: 60, Enabled: enabled,
		})
	}
	add("s1", "n1", "站点 HTTPS", "https", "https://Example.com/health", true)
	add("s2", "n2", "站点 TCP", "tcp", "example.com:443", true)
	add("s3", "", "站点 DNS", "dns", "example.com@1.1.1.1", true)
	add("s4", "n1", "已停用的探针", "https", "https://example.com/", false)
	add("s5", "", "另一站点 A", "https", "https://other.example", true)
	add("s6", "", "另一站点 B", "tcp", "other.example:80", true)
	add("s7", "", "单独目标", "tcp", "lone.example:22", true)
	add("s8", "", "失败站点 A", "tcp", "broken.example:1", true)
	add("s9", "", "失败站点 B", "tcp", "broken.example:2", true)
	add("s10", "", "停用站点 A", "tcp", "off.example:1", false)
	add("s11", "", "停用站点 B", "tcp", "off.example:2", false)

	now := time.Now().Unix()
	for _, r := range []model.ServiceResult{
		{ServiceID: "s1", TS: now - 20, OK: true, LatencyMs: 20, CertDays: 30},
		{ServiceID: "s2", TS: now - 20, OK: false},
		{ServiceID: "s3", TS: now - 20, OK: true, LatencyMs: 5},
		// 停用服务的历史失败结果不能拉低整组判定。
		{ServiceID: "s4", TS: now - 20, OK: false},
		{ServiceID: "s8", TS: now - 20, OK: false},
		{ServiceID: "s9", TS: now - 20, OK: false},
	} {
		st.AddServiceResult(r)
	}

	h := New(st, nil, nil, Config{AdminToken: testAdminToken}).Handler()
	w := adminReq(h, http.MethodGet, "/api/services/compare", "")
	if w.Code != http.StatusOK {
		t.Fatalf("同目标对比应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var got serviceCompareResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Vantage != "control-server" || !strings.Contains(got.Note, "控制服务器") {
		t.Errorf("视角说明缺失: %+v", got)
	}
	if len(got.Groups) != 4 {
		t.Fatalf("应有 4 个多探测主机，实际 %+v", got.Groups)
	}
	if got.Singles != 1 || got.Ungrouped != 0 {
		t.Errorf("单探测/无法归组计数错误: singles=%d ungrouped=%d", got.Singles, got.Ungrouped)
	}
	wantOrder := []string{"broken.example", "example.com", "other.example", "off.example"}
	for i, host := range wantOrder {
		if got.Groups[i].Host != host {
			t.Errorf("第 %d 组应为 %s（异常优先），实际 %+v", i, host, got.Groups)
		}
	}
	byHost := map[string]serviceCompareGroup{}
	for _, g := range got.Groups {
		byHost[g.Host] = g
	}

	main := byHost["example.com"]
	if main.Verdict != "partial" {
		t.Fatalf("example.com 应为 partial，实际 %+v", main)
	}
	if main.Total != 4 || main.Enabled != 3 || main.Probed != 3 || main.Failing != 1 {
		t.Errorf("分组计数错误: %+v", main)
	}
	rows := map[string]serviceCompareRow{}
	for _, row := range main.Services {
		rows[row.ID] = row
	}
	if r := rows["s1"]; r.NodeName != "东京" || !r.Has || !r.OK || r.CertDays != 30 || r.Uptime != 100 {
		t.Errorf("HTTPS 行数据错误: %+v", r)
	}
	if r := rows["s2"]; r.NodeName != "香港" || !r.Has || r.OK || r.Uptime != 0 {
		t.Errorf("TCP 行数据错误: %+v", r)
	}
	if r := rows["s3"]; r.NodeID != "" || r.NodeName != "" {
		t.Errorf("未关联节点应保留空归属: %+v", r)
	}
	if r := rows["s4"]; r.Enabled {
		t.Errorf("停用探针应显示为停用: %+v", r)
	}
	if main.Services[0].ID != "s1" || main.Services[1].ID != "s2" {
		t.Errorf("已归属节点的行应排在未关联之前: %+v", main.Services)
	}
	if main.Services[len(main.Services)-1].ID != "s4" {
		t.Errorf("停用探针应排在最后: %+v", main.Services)
	}

	other := byHost["other.example"]
	if other.Verdict != "unknown" || other.Probed != 0 || other.Failing != 0 {
		t.Errorf("无数据的同目标组应为 unknown: %+v", other)
	}
	broken := byHost["broken.example"]
	if broken.Verdict != "down" || broken.Total != 2 || broken.Probed != 2 || broken.Failing != 2 {
		t.Errorf("全部失败的同目标组应为 down: %+v", broken)
	}
	off := byHost["off.example"]
	if off.Verdict != "disabled" || off.Enabled != 0 || off.Total != 2 {
		t.Errorf("全部停用的同目标组应为 disabled: %+v", off)
	}

	if w := adminReq(h, http.MethodPost, "/api/services/compare", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST 应 405，实际 %d", w.Code)
	}

	r := httptest.NewRequest(http.MethodGet, "/api/services/compare", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("未登录应 401，实际 %d", w.Code)
	}
}

func TestServiceCompareFrontendWiring(t *testing.T) {
	b, err := assets.ReadFile("dist/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	for _, want := range []string{
		"/api/services/compare",
		"同目标并排",
		"所有探测均由控制服务器发起",
		"svc-group-row",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js 缺少同目标对比接线: %q", want)
		}
	}
}

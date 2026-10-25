package web

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/model"
)

// TestOverviewHeroFields 验证总览新增的速率合计与本周期流量合计：
// 速率 = 各节点最近两点差分；周期用量 = 服务端按口径合成的 Used 之和。
func TestOverviewHeroFields(t *testing.T) {
	st := publicStore(t)
	st.UpsertNode(&model.Node{ID: "h1", Name: "hero-1", TrafficQuotaGB: 10, TrafficMode: "sum"})
	st.UpsertNode(&model.Node{ID: "h2", Name: "hero-2", TrafficQuotaGB: 5, TrafficMode: "sum"})
	now := time.Now().Unix()
	// 每台两个点、间隔 10s：h1 每秒收 100B 发 50B；h2 收 20B 发 10B。
	for _, c := range []struct {
		id        string
		r1, t1    uint64
		r2, t2    uint64
		disk1, d2 uint64
	}{
		{"h1", 1000, 500, 2000, 1500, 100, 300},
		{"h2", 200, 100, 1200, 600, 0, 0},
	} {
		m0 := model.Metrics{TS: now - 10, NetRx: c.r1, NetTx: c.t1, DiskReadBytes: c.disk1, CPUPct: 10}
		m1 := model.Metrics{TS: now, NetRx: c.r2, NetTx: c.t2, DiskWriteBytes: c.d2, CPUPct: 20}
		st.AddMetrics(c.id, model.HostInfo{Hostname: c.id}, m0)
		st.AddMetrics(c.id, model.HostInfo{Hostname: c.id}, m1)
	}
	srv, h := scanServer(t)
	srv.st = st
	w := doReq(t, h, http.MethodGet, "/api/overview", "", true)
	if w.Code != 200 {
		t.Fatalf("overview 失败: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		RateIn      uint64 `json:"rateIn"`
		RateOut     uint64 `json:"rateOut"`
		PeriodUsed  uint64 `json:"periodUsed"`
		PeriodQuota uint64 `json:"periodQuota"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// h1: (2000-1000)/10=100, (1500-500)/10=100；h2: (1200-200)/10=100, (600-100)/10=50
	if resp.RateIn != 200 || resp.RateOut != 150 {
		t.Fatalf("速率合计错误: in=%d out=%d", resp.RateIn, resp.RateOut)
	}
	// 周期用量：sum 口径 = 收发增量合计。h1: 1000+1000=2000；h2: 1000+500=1500。
	if resp.PeriodUsed != 3500 {
		t.Fatalf("本周期用量合计错误: %d", resp.PeriodUsed)
	}
	if resp.PeriodQuota != 15*(1<<30) {
		t.Fatalf("配额合计应为 15GiB，实际 %d", resp.PeriodQuota)
	}
}

// TestNodeViewRates 验证 /api/nodes 的单节点速率字段。
func TestNodeViewRates(t *testing.T) {
	st := publicStore(t)
	st.UpsertNode(&model.Node{ID: "r1", Name: "rate-1"})
	now := time.Now().Unix()
	st.AddMetrics("r1", model.HostInfo{}, model.Metrics{TS: now - 30, NetRx: 0, NetTx: 0})
	st.AddMetrics("r1", model.HostInfo{}, model.Metrics{TS: now, NetRx: 3000, NetTx: 1500})
	srv, h := scanServer(t)
	srv.st = st
	w := doReq(t, h, http.MethodGet, "/api/nodes", "", true)
	if w.Code != 200 {
		t.Fatalf("nodes 失败: %d %s", w.Code, w.Body.String())
	}
	var resp []struct {
		ID      string `json:"id"`
		RateIn  uint64 `json:"rateIn"`
		RateOut uint64 `json:"rateOut"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(resp) != 1 || resp[0].ID != "r1" {
		t.Fatalf("节点列表不符: %+v", resp)
	}
	if resp[0].RateIn != 100 || resp[0].RateOut != 50 {
		t.Fatalf("单节点速率错误: %+v", resp[0])
	}
}

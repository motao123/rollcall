package web

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/model"
)

// TestSeriesResponseIncludesFleet 详情页序列响应必须带机群均值字段，
// 且数值来自全部有数据节点的平均（与概览 KPI 同口径：节点平均值的平均）。
func TestSeriesResponseIncludesFleet(t *testing.T) {
	srv, h := scanServer(t)
	srv.st.UpsertNode(&model.Node{ID: "fa", Name: "fa"})
	srv.st.UpsertNode(&model.Node{ID: "fb", Name: "fb"})
	now := time.Now().Unix()
	// 两台节点同一时刻的 CPU：20 与 60 → 机群均值 40
	srv.st.AddMetrics("fa", model.HostInfo{}, model.Metrics{
		TS: now - 60, CPUPct: 20,
		MemUsed: 1 << 30, MemTotal: 4 << 30, DiskUsed: 1 << 30, DiskTotal: 8 << 30,
	})
	srv.st.AddMetrics("fb", model.HostInfo{}, model.Metrics{
		TS: now - 60, CPUPct: 60,
		MemUsed: 2 << 30, MemTotal: 4 << 30, DiskUsed: 2 << 30, DiskTotal: 8 << 30,
	})

	w := doReq(t, h, http.MethodGet, "/api/nodes/fa/series?range=1h", "", true)
	if w.Code != 200 {
		t.Fatalf("series 失败: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Fleet []struct {
			TS   int64   `json:"ts"`
			CPU  float64 `json:"cpu"`
			Mem  float64 `json:"mem"`
			Disk float64 `json:"disk"`
		} `json:"fleet"`
		Points []struct {
			TS int64 `json:"ts"`
		} `json:"points"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(resp.Fleet) == 0 {
		t.Fatal("响应应包含 fleet 机群均值序列")
	}
	// 机群均值 = (20+60)/2 = 40
	found := false
	for _, p := range resp.Fleet {
		if p.CPU >= 39.9 && p.CPU <= 40.1 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("机群均值应为 40，实际: %+v", resp.Fleet)
	}
	// 机群序列的时间戳应与该节点自己的原始点对齐（同 1 分钟桶）
	nodeTs := resp.Points[len(resp.Points)-1].TS
	bucketTs := nodeTs - nodeTs%60
	aligned := false
	for _, p := range resp.Fleet {
		if p.TS == bucketTs {
			aligned = true
			break
		}
	}
	if !aligned {
		t.Fatalf("机群序列应对齐到节点最新点的分钟桶 %d", bucketTs)
	}
}

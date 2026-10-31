package store

import (
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/model"
)

// TestFleetSeriesTiered 聚合层机群平均：两台节点同桶平均、单节点桶自成一路。
func TestFleetSeriesTiered(t *testing.T) {
	st := New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "f1", Name: "f1"})
	st.UpsertNode(&model.Node{ID: "f2", Name: "f2"})
	st.UpsertNode(&model.Node{ID: "f3", Name: "f3"}) // 只在第二个桶出现
	now := time.Now().Truncate(time.Minute).Unix()

	// 桶 1（since 对齐）：f1=20, f2=40 → 均值 30
	// 桶 2：f1=60, f2=80, f3=100 → 均值 80
	mk := func(id string, ts int64, cpu float64) {
		st.AddMetrics(id, model.HostInfo{}, model.Metrics{
			TS: ts, CPUPct: cpu,
			MemUsed: 1 << 30, MemTotal: 4 << 30,
			DiskUsed: 1 << 30, DiskTotal: 8 << 30,
		})
	}
	mk("f1", now-3600, 20)
	mk("f2", now-3600, 40)
	mk("f1", now-1800, 60)
	mk("f2", now-1800, 80)
	mk("f3", now-1800, 100)

	fleet := st.FleetSeries("1m", now-7200)
	if len(fleet) < 2 {
		t.Fatalf("机群序列应至少 2 个点，实际 %d", len(fleet))
	}
	// 找 CPU=30 的桶（桶 1：20/40 的均值）
	found30, found80 := false, false
	for _, p := range fleet {
		if p.CPU >= 29.9 && p.CPU <= 30.1 {
			found30 = true
		}
		if p.CPU >= 79.9 && p.CPU <= 80.1 {
			found80 = true
		}
	}
	if !found30 || !found80 {
		t.Fatalf("应找到均值 30 与 80 的桶，实际: %+v", fleet)
	}
}

// TestFleetSeriesRawBuckets 原始环路径：跨节点不对齐的时间戳按 1 分钟桶归约。
func TestFleetSeriesRawBuckets(t *testing.T) {
	st := New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "r1", Name: "r1"})
	st.UpsertNode(&model.Node{ID: "r2", Name: "r2"})
	base := time.Now().Truncate(time.Minute).Unix()
	// r1 在 :00 报，r2 在 :30 报（不对齐），同分钟内 CPU 20 与 60 → 机群均值 40
	st.AddMetrics("r1", model.HostInfo{}, model.Metrics{TS: base, CPUPct: 20, MemUsed: 1 << 30, MemTotal: 4 << 30, DiskUsed: 1 << 30, DiskTotal: 8 << 30})
	st.AddMetrics("r2", model.HostInfo{}, model.Metrics{TS: base + 30, CPUPct: 60, MemUsed: 2 << 30, MemTotal: 4 << 30, DiskUsed: 2 << 30, DiskTotal: 8 << 30})
	fleet := st.FleetSeries(HistoryResolutionRaw, base-60)
	if len(fleet) == 0 {
		t.Fatal("原始路径应产出至少 1 个机群点")
	}
	p := fleet[0]
	if p.CPU < 39.9 || p.CPU > 40.1 {
		t.Fatalf("机群均值应为 40（20/60 的均值），实际 %v", p.CPU)
	}
}

// TestFleetSeriesSkipsNodesWithoutData 无数据节点不参与平均。
func TestFleetSeriesSkipsNodesWithoutData(t *testing.T) {
	st := New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "w", Name: "w"}) // 无任何上报
	st.UpsertNode(&model.Node{ID: "d", Name: "d"})
	st.AddMetrics("d", model.HostInfo{}, model.Metrics{
		TS: time.Now().Unix() - 120, CPUPct: 50,
		MemUsed: 1 << 30, MemTotal: 4 << 30, DiskUsed: 1 << 30, DiskTotal: 8 << 30,
	})
	fleet := st.FleetSeries("1m", time.Now().Unix()-3600)
	for _, p := range fleet {
		if p.CPU != 50 {
			t.Fatalf("无数据节点不应拉低均值，实际 %v", p.CPU)
		}
	}
}

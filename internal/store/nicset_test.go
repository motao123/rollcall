package store

import (
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/model"
)

// TestNICSetChangeRebuildsBaseline 网卡集合变化（WARP/多 WAN 切换）时：
// 跳变部分绝不能记成真实流量；标记周期不完整；新集合上的后续增量正常累计。
func TestNICSetChangeRebuildsBaseline(t *testing.T) {
	st := New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "n1", TrafficQuotaGB: 100, TrafficMode: "sum"})
	now := time.Now().Unix()

	// 基线：集合 A 上累计 1000
	st.AddMetrics("n1", model.HostInfo{}, model.Metrics{TS: now - 30, NetRx: 600, NetTx: 400}, "fpA")
	st.AddMetrics("n1", model.HostInfo{}, model.Metrics{TS: now - 20, NetRx: 1100, NetTx: 900}, "fpA")
	if u := st.TrafficUsage("n1"); u.Used != 1000 || u.Incomplete {
		t.Fatalf("集合 A 内增量应正常累计 1000，实际 used=%d incomplete=%v", u.Used, u.Incomplete)
	}

	// 集合切换到 B：读数跳到 999,999,000（例如新增了 WARP 网卡，计数器口径变了）
	st.AddMetrics("n1", model.HostInfo{}, model.Metrics{TS: now - 10, NetRx: 899999600, NetTx: 99999500}, "fpB")
	u := st.TrafficUsage("n1")
	if u.Used != 1000 {
		t.Fatalf("切换瞬间的跳变不应计入用量，期望仍 1000，实际 %d", u.Used)
	}
	if !u.Incomplete {
		t.Fatal("集合切换后周期应标记不完整")
	}

	// 新集合上的正常增量继续累计（rx +1000、tx +1000 → used 1000+500+500+1000+1000? ）
	// 精确算：切换前 usedRx=500 usedTx=500；切换重建基线不清用量；
	// 新样本 delta rx=1000 tx=1000 → usedRx=1500 usedTx=1500 → sum=3000。
	st.AddMetrics("n1", model.HostInfo{}, model.Metrics{TS: now, NetRx: 900000600, NetTx: 100000500}, "fpB")
	if u := st.TrafficUsage("n1"); u.Used != 3000 {
		t.Fatalf("新集合上的增量应正常累计 3000，实际 %d", u.Used)
	}

	// 校正本期清除不完整标记
	st.AdjustTrafficUsed("n1", 500)
	if u := st.TrafficUsage("n1"); u.Incomplete || u.Used != 500 {
		t.Fatalf("校正后应 used=500 且 incomplete=false，实际 %d/%v", u.Used, u.Incomplete)
	}
}

// TestNICSetFirstReportNoReset 首次上报（旧值为空）只记录指纹，不触发重置路径。
func TestNICSetFirstReportNoReset(t *testing.T) {
	st := New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n2", Name: "n2"})
	now := time.Now().Unix()
	st.AddMetrics("n2", model.HostInfo{}, model.Metrics{TS: now - 20, NetRx: 100, NetTx: 100}, "fpX")
	st.AddMetrics("n2", model.HostInfo{}, model.Metrics{TS: now, NetRx: 300, NetTx: 300}, "fpX")
	if u := st.TrafficUsage("n2"); u.Used != 400 || u.Incomplete {
		t.Fatalf("正常同集合累计不应受影响，实际 used=%d incomplete=%v", u.Used, u.Incomplete)
	}
	// 无指纹（旧 Agent）路径完全不受影响
	st.AddMetrics("n2", model.HostInfo{}, model.Metrics{TS: now + 30, NetRx: 500, NetTx: 500})
	if u := st.TrafficUsage("n2"); u.Used != 800 {
		t.Fatalf("旧 Agent（无指纹）上报应正常累计，实际 %d", u.Used)
	}
}

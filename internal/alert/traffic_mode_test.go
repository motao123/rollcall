package alert

import (
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/store"
)

// TestTrafficQuotaUsesNodeMode 回归：
// 配额告警曾固定按 UsedRx+UsedTx 计算，节点配置成单边计费后
// 用量被高估一倍，商家按 1.9GB/2GB（95%）还没超限，面板已经报 190%。
func TestTrafficQuotaUsesNodeMode(t *testing.T) {
	const gib = uint64(1) << 30

	// setup 造一个 2GB 配额、上下行各 1.9GB 的节点。
	setup := func(mode string) *store.Store {
		st := store.New(t.TempDir())
		st.UpsertNode(&model.Node{
			ID: "n1", Name: "节点一", Token: "t",
			TrafficQuotaGB: 2, TrafficResetDay: 1, TrafficMode: mode,
		})
		now := time.Now().Unix()
		st.AddMetrics("n1", model.HostInfo{}, model.Metrics{TS: now - 120})
		// 1.9 GiB（用整数运算，避免浮点误差影响 95% 的边界判断）。
		rx := gib * 19 / 10
		st.AddMetrics("n1", model.HostInfo{}, model.Metrics{
			TS: now - 60, NetRx: rx, NetTx: rx,
		})
		st.UpsertRule(&model.AlertRule{
			ID: "rq", Name: "流量配额", Metric: "traffic_quota", Op: "gt",
			Threshold: 100, Duration: 0, Severity: "warn", Enabled: true,
		})
		return st
	}

	// 合计口径：3.8GB / 2GB = 190%，必须触发。
	sum := setup(model.TrafficModeSum)
	New(sum).Evaluate()
	if got := sum.ActiveAlertCount(); got != 1 {
		t.Fatalf("合计口径应触发配额告警，实际 %d 条", got)
	}

	// 三种单边/取大口径：1.9GB / 2GB = 95%，都不该触发。
	for _, mode := range []string{model.TrafficModeRx, model.TrafficModeTx, model.TrafficModeMax} {
		st := setup(mode)
		New(st).Evaluate()
		if got := st.ActiveAlertCount(); got != 0 {
			t.Errorf("口径 %s 下用量 95%% 不应触发配额告警，实际 %d 条", mode, got)
		}
	}
}

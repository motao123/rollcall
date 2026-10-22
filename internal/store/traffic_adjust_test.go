package store

import (
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/model"
)

// TestAdjustTrafficUsedHonorsMode 覆盖"人工校正本周期用量"：
// 校正后 Used 必须恰好等于给定量，且后续上报的增量要在它之上累加
// （不能把 LastRx/LastTx 基线一起清掉，否则下一次上报会再算一遍）。
func TestAdjustTrafficUsedHonorsMode(t *testing.T) {
	const gb = uint64(1024 * 1024 * 1024)
	cases := []struct {
		mode string
		want uint64 // 校正后 Used
		add  uint64 // 再上报的增量（只加下行）
	}{
		{"", 5 * gb, 7},    // 默认 sum
		{"sum", 5 * gb, 7}, // 收发相加
		{"rx", 5 * gb, 7},  // 只算下行 → 增量计入
		{"tx", 5 * gb, 0},  // 只算上行 → 下行增量不计入
		{"max", 5 * gb, 7}, // 取较大者 → 下行变大后计入
	}

	for _, tc := range cases {
		t.Run("mode="+tc.mode, func(t *testing.T) {
			st := New(t.TempDir())
			st.UpsertNode(&model.Node{
				ID: "n1", Name: "机-1", TrafficResetDay: 1, TrafficMode: tc.mode,
				TrafficQuotaGB: 200,
			})
			now := time.Now().Unix()
			// 先制造一个周期：一次上报（基线 1000/500）
			st.AddMetrics("n1", model.HostInfo{}, model.Metrics{TS: now, NetRx: 1000, NetTx: 500})

			usage, ok := st.AdjustTrafficUsed("n1", 5*gb)
			if !ok {
				t.Fatal("校正失败：节点不存在？")
			}
			if usage.Used != 5*gb {
				t.Fatalf("校正后 Used = %d，期望 %d", usage.Used, 5*gb)
			}
			if usage.QuotaGB != 200 {
				t.Fatalf("用量视图丢了配额: %+v", usage)
			}

			// 再上报一次：只在原基线上加 tc.add 字节下行
			st.AddMetrics("n1", model.HostInfo{},
				model.Metrics{TS: now + 30, NetRx: 1000 + tc.add, NetTx: 500})
			after := st.TrafficUsage("n1")
			if after.Used != 5*gb+tc.add {
				t.Fatalf("增量应累加在校正值上：Used = %d，期望 %d", after.Used, 5*gb+tc.add)
			}
		})
	}
}

// TestAdjustTrafficUsedUnknownNode 确认未知节点不会凭空造出周期。
func TestAdjustTrafficUsedUnknownNode(t *testing.T) {
	st := New(t.TempDir())
	if _, ok := st.AdjustTrafficUsed("nope", 1<<30); ok {
		t.Fatal("未知节点应返回 false")
	}
}

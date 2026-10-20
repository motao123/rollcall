package model

import "testing"

// TestTrafficUsedModes 覆盖四种计费口径。
// 单边口径算错会直接导致配额告警提前或延后触发，
// 而这类错误在界面上只表现为"百分比不对"，很难靠肉眼发现。
func TestTrafficUsedModes(t *testing.T) {
	cases := []struct {
		mode     string
		rx, tx   uint64
		want     uint64
		wantNorm string
	}{
		{"", 100, 40, 140, TrafficModeSum}, // 未设置按默认合计
		{TrafficModeSum, 100, 40, 140, TrafficModeSum},
		{TrafficModeRx, 100, 40, 100, TrafficModeRx},
		{TrafficModeTx, 100, 40, 40, TrafficModeTx},
		{TrafficModeMax, 100, 40, 100, TrafficModeMax},
		{TrafficModeMax, 40, 100, 100, TrafficModeMax},
		{TrafficModeRx, 0, 0, 0, TrafficModeRx},
	}
	for _, c := range cases {
		if got := TrafficUsed(c.mode, c.rx, c.tx); got != c.want {
			t.Errorf("TrafficUsed(%q,%d,%d)=%d, want %d", c.mode, c.rx, c.tx, got, c.want)
		}
		if got := NormalizeTrafficMode(c.mode); got != c.wantNorm {
			t.Errorf("NormalizeTrafficMode(%q)=%q, want %q", c.mode, got, c.wantNorm)
		}
	}
}

// TestValidTrafficMode 保证未知口径在入库前被拒，
// 而不是被静默当成 sum —— 拼错 "tx " 这种错误会让用户以为改了口径。
func TestValidTrafficMode(t *testing.T) {
	for _, ok := range []string{"", "sum", "rx", "tx", "max"} {
		if !ValidTrafficMode(ok) {
			t.Errorf("ValidTrafficMode(%q) 应为 true", ok)
		}
	}
	// 大小写不在模型层放宽：HTTP 层会先 lower 再校验。
	for _, bad := range []string{"SUM", "u", "both", "sum tx", " max"} {
		if ValidTrafficMode(bad) {
			t.Errorf("ValidTrafficMode(%q) 应为 false", bad)
		}
	}
}

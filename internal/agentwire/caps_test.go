package agentwire

import "testing"

// TestNormalizeCaps 规整能力清单：小写、去空、去重、排序，空清单落到 none。
func TestNormalizeCaps(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{nil, CapsNone},
		{[]string{}, CapsNone},
		{[]string{"temp"}, "temp"},
		{[]string{"Temp", " temp ", ""}, "temp"},
		{[]string{"diskio", "temp", "temp"}, "diskio,temp"},
		// none 只是哨兵，不能当成一项真实能力混进清单
		{[]string{"none"}, CapsNone},
		{[]string{"none", "temp"}, "temp"},
	}
	for _, c := range cases {
		if got := NormalizeCaps(c.in); got != c.want {
			t.Errorf("NormalizeCaps(%v) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestParseCaps 是本功能的核心契约：**头缺席**与**已确认无能力**必须分开。
// 两者混成一个空值时，面板就无法区分"机器没有传感器"与"Agent 还没升级"。
func TestParseCaps(t *testing.T) {
	cases := []struct {
		header string
		want   string
		known  bool
	}{
		{"", "", false},            // 旧 Agent：未知
		{"   ", "", false},         // 空白同样视为未上报
		{CapsNone, CapsNone, true}, // 新 Agent 已探测，无能力
		{"none", CapsNone, true},
		{"temp", "temp", true},
		{" Temp , DiskIO ", "diskio,temp", true},
	}
	for _, c := range cases {
		got, known := ParseCaps(c.header)
		if got != c.want || known != c.known {
			t.Errorf("ParseCaps(%q) = (%q, %v)，期望 (%q, %v)", c.header, got, known, c.want, c.known)
		}
	}
}

// TestParseCapsTruncates 超长头被截断而不是被拒：
// 能力是枚举值，异常 Agent 不应能用超长头把面板字段撑爆。
func TestParseCapsTruncates(t *testing.T) {
	long := ""
	for i := 0; i < 200; i++ {
		long += "a"
	}
	got, known := ParseCaps(long)
	if !known {
		t.Fatal("超长头仍应被识别为已知")
	}
	if len(got) > maxCapsLen {
		t.Fatalf("规整结果长度 %d 超过上限 %d", len(got), maxCapsLen)
	}
}

// TestHasCaps 覆盖三种"没有"：未上报、确认无能力、清单不含该项。
func TestHasCaps(t *testing.T) {
	if HasCaps("", "temp") {
		t.Error("未上报时不应声称有温度能力")
	}
	if HasCaps(CapsNone, "temp") {
		t.Error("确认无能力时不应声称有温度能力")
	}
	if HasCaps("diskio", "temp") {
		t.Error("清单不含 temp 时不应命中")
	}
	if !HasCaps("diskio,temp", "temp") {
		t.Error("清单含 temp 时应命中")
	}
}

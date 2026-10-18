package main

import (
	"reflect"
	"testing"
)

// TestStripFlagNormalizesDashes 防止 --rollback 未被剥离导致的无限回滚循环。
//
// Go 的 flag 包把 -rollback 与 --rollback 视为同一开关，因此剥离逻辑
// 必须归一化前导横线；旧实现只精确匹配 "-rollback"，双横线形式会漏掉，
// re-exec 后的进程再次回滚、再次 re-exec，起不来。
func TestStripFlagNormalizesDashes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"单横线", []string{"agent", "-rollback"}, []string{"agent"}},
		{"双横线", []string{"agent", "--rollback"}, []string{"agent"}},
		{"单横线带值", []string{"agent", "-rollback=true"}, []string{"agent"}},
		{"双横线带值", []string{"agent", "--rollback=true"}, []string{"agent"}},
		{
			"保留其它参数",
			[]string{"agent", "--rollback", "-config=/etc/rollcall/agent.json", "extra"},
			[]string{"agent", "-config=/etc/rollcall/agent.json", "extra"},
		},
		{"前缀相同的长参数不误删", []string{"agent", "-rollbacks"}, []string{"agent", "-rollbacks"}},
		{"位置参数不误删", []string{"agent", "rollback"}, []string{"agent", "rollback"}},
		{"仅横线不误删", []string{"agent", "-"}, []string{"agent", "-"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stripFlag(c.args, "-rollback"); !reflect.DeepEqual(got, c.want) {
				t.Errorf("stripFlag(%v) = %v, want %v", c.args, got, c.want)
			}
		})
	}
}

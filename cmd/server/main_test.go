package main

import (
	"testing"

	"github.com/motao123/rollcall/internal/alert"
	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/store"
)

// TestEnsureDefaultRulesDoesNotResurrect 回归测试：
// 用户删光默认规则后重启，不能因为「规则数为 0」又播种一遍。
func TestEnsureDefaultRulesDoesNotResurrect(t *testing.T) {
	st := store.New(t.TempDir())
	ensureDefaultRules(st)
	if got, want := len(st.Rules()), len(alert.DefaultRules()); got != want {
		t.Fatalf("首次启动应播种 %d 条默认规则，实际 %d", want, got)
	}
	if st.GetSetting("rulesInitialized") == "" {
		t.Fatal("未写入初始化标记")
	}

	for _, r := range st.Rules() {
		st.DeleteRule(r.ID)
	}
	ensureDefaultRules(st)
	if got := len(st.Rules()); got != 0 {
		t.Fatalf("删光规则后重启不应复活默认规则，实际 %d 条", got)
	}
}

// TestEnsureDefaultRulesCompletesPartialSeed 标记缺失代表上次播种未完整结束：
// 已有自定义规则可以保留，但缺失的默认规则要补齐，且不能重复添加已有的。
func TestEnsureDefaultRulesCompletesPartialSeed(t *testing.T) {
	st := store.New(t.TempDir())
	st.UpsertRule(&model.AlertRule{ID: "custom", Name: "自定义", Metric: "cpu", Enabled: true})
	st.UpsertRule(&model.AlertRule{ID: "r_cpu", Name: "已存在", Metric: "cpu", Enabled: true})
	ensureDefaultRules(st)
	if got, want := len(st.Rules()), len(alert.DefaultRules())+1; got != want {
		t.Fatalf("1 条自定义 + %d 条默认规则应为 %d 条，实际 %d 条", len(alert.DefaultRules()), want, got)
	}
	var cpu *model.AlertRule
	for _, r := range st.Rules() {
		if r.ID == "r_cpu" {
			cpu = r
		}
	}
	if cpu == nil || cpu.Name != "已存在" {
		t.Fatalf("已有的默认规则不应被覆盖: %+v", cpu)
	}
	if st.GetSetting("rulesInitialized") == "" {
		t.Fatal("补齐后应写入初始化标记")
	}
}

func TestIsLoopbackListen(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:9080", true},
		{"127.0.0.2:9080", true},
		{"[::1]:9080", true},
		{"localhost:9080", true},
		// 全接口写法与具体网卡地址都必须视为对外暴露
		{":9080", false},
		{"0.0.0.0:9080", false},
		{"192.168.1.10:9080", false},
		{"garbage", false},
	}
	for _, c := range cases {
		if got := isLoopbackListen(c.addr); got != c.want {
			t.Errorf("isLoopbackListen(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}

package store

import (
	"testing"

	"github.com/motao/rollcall/internal/model"
)

// TestRulesReturnsIndependentSlices 是回归测试。
//
// AlertRule 是唯一带引用字段（NodeIDs / Groups）的持久化结构。
// 只写 `cp := *v` 时切片共享底层数组：调用方 append 会写回库里的规则，
// 而 Save 的快照在锁外序列化，会与这类修改并发撞车。
func TestRulesReturnsIndependentSlices(t *testing.T) {
	st := New(t.TempDir())
	st.UpsertRule(&model.AlertRule{
		ID: "r1", Name: "规则", Metric: "cpu", Op: "gt", Threshold: 90,
		NodeIDs: []string{"n1"}, Groups: []string{"prod"},
	})

	got := st.Rules()
	if len(got) != 1 {
		t.Fatalf("规则数 = %d", len(got))
	}
	// 改返回值不应影响库内规则
	got[0].NodeIDs[0] = "hacked"
	got[0].Groups = append(got[0].Groups, "extra")

	again := st.Rules()
	if again[0].NodeIDs[0] != "n1" {
		t.Fatalf("NodeIDs 被写回库内规则: %v", again[0].NodeIDs)
	}
	if len(again[0].Groups) != 1 {
		t.Fatalf("Groups 被写回库内规则: %v", again[0].Groups)
	}

	// 快照同样必须是独立副本：改返回值后保存，落盘内容应保持原值
	if err := st.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	re := New(t.TempDir())
	_ = re
	if got[0].NodeIDs[0] != "hacked" {
		t.Fatal("测试自身的修改未生效，用例失效")
	}
}

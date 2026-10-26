package store

import (
	"testing"

	"github.com/motao123/rollcall/internal/model"
)

// seedServiceRule 建一条服务类规则（service_down）。
func seedServiceRule(t *testing.T, st *Store, id string) {
	t.Helper()
	st.UpsertRule(&model.AlertRule{
		ID: id, Name: "服务失联", Metric: "service_down", Op: "gt",
		Threshold: 0, Severity: "warn", Enabled: true,
	})
}

// TestServiceAlertFollowsReparent 是 A3 的回归测试。
//
// 早先落库查重按 (规则, 节点, 服务) 比对，而引擎的键是 rule|service（不含节点）。
// 服务改挂到另一台节点后，引擎在同一条键上继续评估：旧的活跃告警（旧节点）
// 不会被匹配到，于是同一问题开出第二条告警 —— 旧的既不自愈也不关闭，
// 面板上永远挂着两条。正确语义：服务告警以**服务**为单位，改挂节点是
// "同一条告警换了位置"，原地更新节点字段。
func TestServiceAlertFollowsReparent(t *testing.T) {
	st := New(t.TempDir())
	seedServiceRule(t, st, "r_svc")
	st.UpsertNode(&model.Node{ID: "n1", Name: "旧节点"})
	st.UpsertNode(&model.Node{ID: "n2", Name: "新节点"})
	if !st.CreateService(&model.Service{ID: "s1", NodeID: "n1", Name: "探测目标",
		Type: "http", Target: "https://example.com/", Interval: 60, Enabled: true}) {
		t.Fatal("创建服务失败")
	}

	a1, ok := st.OpenAlert(&model.Alert{ID: "a1", RuleID: "r_svc", RuleName: "服务失联",
		NodeID: "n1", NodeName: "旧节点", ServiceID: "s1", ServiceName: "探测目标",
		Severity: "warn", Value: 1})
	if !ok {
		t.Fatal("开告警失败")
	}
	_ = a1
	if got := st.ActiveAlertCount(); got != 1 {
		t.Fatalf("应 1 条活跃告警，实际 %d", got)
	}

	// 服务改挂节点（引擎下轮评估会带新节点的身份调用 OpenAlert）
	sv, _ := st.Service("s1")
	sv.NodeID = "n2"
	st.UpsertService(sv)

	created, ok := st.OpenAlert(&model.Alert{ID: "a2", RuleID: "r_svc", RuleName: "服务失联",
		NodeID: "n2", NodeName: "新节点", ServiceID: "s1", ServiceName: "探测目标",
		Severity: "warn", Value: 1})
	if created || !ok {
		t.Fatalf("改挂节点应命中原告警（created=%v ok=%v），不能新开一条", created, ok)
	}

	active := st.ActiveAlerts()
	if len(active) != 1 {
		t.Fatalf("改挂节点后应仍只有 1 条活跃告警，实际 %d 条", len(active))
	}
	if active[0].ID != "a1" {
		t.Fatalf("应复用原告警 ID（通知任务靠它关联），实际 %q", active[0].ID)
	}
	if active[0].NodeID != "n2" || active[0].NodeName != "新节点" {
		t.Fatalf("告警应更新为新节点，实际 node=%q name=%q", active[0].NodeID, active[0].NodeName)
	}
}

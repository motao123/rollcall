package alert

import (
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/store"
)

// TestExemptCoversServiceRules 台账级豁免必须同时覆盖服务类规则。
//
// 设置项写的是「该节点不再评估任何指标规则」，实际只豁免了 CPU/内存这类
// 指标，探测失败照样报 —— 用户勾了豁免准备重启机器，结果仍然被
// service_down 通知叫醒。
func TestExemptCoversServiceRules(t *testing.T) {
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "e1", Name: "豁免节点", AlertExempt: true})
	if !st.CreateService(&model.Service{
		ID: "sv1", NodeID: "e1", Name: "站点", Type: "http",
		Target: "https://example.com/", Interval: 60, Enabled: true,
	}) {
		t.Fatal("创建探测目标失败")
	}
	st.UpsertRule(&model.AlertRule{
		ID: "r_sd", Name: "服务不可用", Metric: "service_down", Op: "gt",
		Threshold: 1, Severity: model.LevelWarn, Enabled: true,
	})

	// 先制造一条活跃的服务告警（模拟豁免之前已经在报警）
	a := &model.Alert{
		ID: "a1", RuleID: "r_sd", RuleName: "服务不可用",
		NodeID: "e1", NodeName: "豁免节点", ServiceID: "sv1", ServiceName: "站点",
		Severity: model.LevelWarn,
	}
	if created, ok := st.OpenAlert(a); !ok || !created {
		t.Fatalf("应能开立服务告警（created=%v ok=%v）", created, ok)
	}

	eng := New(st)
	eng.Evaluate()

	for _, act := range st.ActiveAlerts() {
		if act.ServiceID == "sv1" {
			t.Fatalf("豁免节点上的服务告警应被关闭，实际仍活跃: %s", act.Message)
		}
	}
	// 告警必须被真正关闭（ResolvedAt 非 0），否则它会永远挂在「进行中」。
	resolved := false
	for _, h := range st.Alerts(50) {
		if h.ID == "a1" && h.ResolvedAt != 0 {
			resolved = true
		}
	}
	if !resolved {
		t.Fatal("豁免时已存在的服务告警应被关闭")
	}
	_ = time.Now
}

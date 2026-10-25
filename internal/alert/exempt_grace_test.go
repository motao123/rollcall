package alert

import (
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/store"
)

// TestAlertExemptSuppressesAndCloses 台账级告警豁免：
// ① 开启后该节点不产生任何新告警；② 已有活跃告警被以「节点告警豁免」关闭；
// ③ 未豁免的节点照常告警。
func TestAlertExemptSuppressesAndCloses(t *testing.T) {
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "x1", Name: "豁免节点", AlertExempt: true})
	st.UpsertNode(&model.Node{ID: "x2", Name: "正常节点"})
	st.UpsertRule(&model.AlertRule{
		ID: "r_off", Name: "节点失联", Metric: "offline", Op: "gt",
		Threshold: 1, Duration: 0, Severity: model.LevelCritical, Enabled: true,
	})
	eng := New(st)
	eng.Evaluate()

	active := st.ActiveAlerts()
	byNode := map[string]bool{}
	for _, a := range active {
		byNode[a.NodeID] = true
	}
	if byNode["x1"] {
		t.Error("豁免节点不应产生告警")
	}
	if !byNode["x2"] {
		t.Error("未豁免节点失联应正常告警")
	}

	// 已有告警后开启豁免 → 下一轮以豁免为由关闭
	st.UpsertNode(&model.Node{ID: "x2", Name: "正常节点", AlertExempt: true})
	eng.Evaluate()
	for _, a := range st.ActiveAlerts() {
		if a.NodeID == "x2" {
			t.Errorf("开启豁免后活跃告警应被关闭，仍在: %s", a.Message)
		}
	}
}

// TestOfflineGraceFloor 全局离线宽限下限：只抬高不缩短。
func TestOfflineGraceFloor(t *testing.T) {
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "g1", Name: "g1"})
	rule := &model.AlertRule{
		ID: "r_off", Name: "节点失联", Metric: "offline", Op: "gt",
		Threshold: 1, Duration: 60, Severity: model.LevelCritical, Enabled: true,
	}
	st.UpsertRule(rule)
	eng := New(st)

	key := rule.ID + "|g1"
	now := time.Now().Unix()

	// 未配置时按规则 Duration
	eng.SetOfflineGrace(0)
	if got := eng.requiredDuration(rule, key, now, false); got != 60 {
		t.Errorf("未配置宽限时应为规则 Duration 60，实际 %d", got)
	}
	// 宽限 300 抬高到 300
	eng.SetOfflineGrace(300)
	if got := eng.requiredDuration(rule, key, now, false); got != 300 {
		t.Errorf("宽限应把 60 抬到 300，实际 %d", got)
	}
	// 宽限 30 不缩短
	eng.SetOfflineGrace(30)
	if got := eng.requiredDuration(rule, key, now, false); got != 60 {
		t.Errorf("宽限小于规则时长时不应缩短，期望 60，实际 %d", got)
	}
	// 负数视为关闭
	eng.SetOfflineGrace(-5)
	if got := eng.requiredDuration(rule, key, now, false); got != 60 {
		t.Errorf("负数宽限应被钳为不干预，期望 60，实际 %d", got)
	}
	// 非离线规则不受影响
	res := &model.AlertRule{ID: "r_cpu", Name: "CPU", Metric: "cpu", Op: "gt",
		Threshold: 90, Duration: 30, Severity: model.LevelWarn, Enabled: true}
	eng.SetOfflineGrace(300)
	if got := eng.requiredDuration(res, "r_cpu|g1", now, false); got != 30 {
		t.Errorf("非离线规则不应受宽限影响，期望 30，实际 %d", got)
	}
}

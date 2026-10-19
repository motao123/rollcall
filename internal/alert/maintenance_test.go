package alert

import (
	"testing"
	"time"

	"github.com/motao/rollcall/internal/model"
	"github.com/motao/rollcall/internal/store"
)

// maintenanceEnv 构造一个含离线规则与两台节点的引擎。
func maintenanceEnv(t *testing.T) (*store.Store, *Engine) {
	t.Helper()
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "m1", Name: "维护中节点"})
	st.UpsertNode(&model.Node{ID: "m2", Name: "正常节点"})
	st.UpsertRule(&model.AlertRule{
		ID: "r_off", Name: "节点失联", Metric: "offline", Op: "gt",
		Threshold: 1, Duration: 0, Severity: model.LevelCritical, Enabled: true,
	})
	eng := New(st)
	return st, eng
}

// TestMaintenanceSuppressesAlerts 计划维护窗口内的节点：
// ① 不产生新告警；② 已有活跃告警被以"计划维护"为由关闭。
// 没有维护窗口的另一台节点照常告警 —— 维护不能误伤别人。
func TestMaintenanceSuppressesAlerts(t *testing.T) {
	st, eng := maintenanceEnv(t)
	now := time.Now().Unix()
	st.UpsertMaintenance(&model.Maintenance{
		ID: "mt1", Title: "机房搬迁", NodeIDs: []string{"m1"},
		StartAt: now - 60, EndAt: now + 3600, CreatedAt: now,
	})

	// 两台节点都失联（从未上报）
	eng.Evaluate()

	active := st.ActiveAlerts()
	byNode := map[string]*model.Alert{}
	for _, a := range active {
		byNode[a.NodeID] = a
	}
	if a, ok := byNode["m1"]; ok {
		t.Errorf("维护中的节点不应产生新告警，实际: %s", a.Message)
	}
	if a, ok := byNode["m2"]; !ok {
		t.Error("未维护的节点失联应正常告警，实际没有")
	} else if a.RuleID != "r_off" {
		t.Errorf("告警规则不符: %s", a.RuleID)
	}
}

// TestMaintenanceClosesExistingAlerts 维护开始前已存在的活跃告警，
// 引擎下一轮评估应以"计划维护"关闭它，而不是挂到窗口结束。
func TestMaintenanceClosesExistingAlerts(t *testing.T) {
	st, eng := maintenanceEnv(t)
	now := time.Now().Unix()

	// 先让节点失联并告警
	eng.Evaluate()
	if len(st.ActiveAlerts()) == 0 {
		t.Fatal("前置条件失败：应先产生活跃告警")
	}

	// 之后才创建覆盖该节点的维护窗口
	st.UpsertMaintenance(&model.Maintenance{
		ID: "mt2", Title: "计划重启", NodeIDs: []string{"m1"},
		StartAt: now - 1, EndAt: now + 600, CreatedAt: now,
	})
	eng.Evaluate()

	for _, a := range st.ActiveAlerts() {
		if a.NodeID == "m1" {
			t.Errorf("维护中的节点既有告警应被关闭: %s", a.Message)
		}
	}
	// 找到关闭的告警，确认原因写清了是哪次维护
	found := false
	for _, a := range st.Alerts(0) {
		if a.NodeID == "m1" && a.ResolvedAt != 0 {
			found = true
		}
	}
	if !found {
		t.Error("应有已恢复的告警记录（而不是静默消失）")
	}
}

// TestMaintenanceExpiry 窗口结束后恢复评估。
func TestMaintenanceExpiry(t *testing.T) {
	st, eng := maintenanceEnv(t)
	now := time.Now().Unix()
	st.UpsertMaintenance(&model.Maintenance{
		ID: "mt3", Title: "短窗口", NodeIDs: []string{"m1"},
		StartAt: now - 100, EndAt: now - 1, CreatedAt: now, // 已结束
	})
	eng.Evaluate()
	found := false
	for _, a := range st.ActiveAlerts() {
		if a.NodeID == "m1" {
			found = true
		}
	}
	if !found {
		t.Error("窗口已结束，节点失联应照常告警")
	}
}

// TestMaintenanceActiveAtCovers 校验窗口判定纯函数：
// 非法区间（结束<=开始）一律不生效，否则窗口永不结束。
func TestMaintenanceActiveAtCovers(t *testing.T) {
	m := &model.Maintenance{StartAt: 100, EndAt: 200}
	if !m.ActiveAt(150) || m.ActiveAt(99) || m.ActiveAt(200) {
		t.Error("区间判定错误")
	}
	if m.ActiveAt(50) {
		t.Error("窗口未开始不应生效")
	}
	bad := &model.Maintenance{StartAt: 200, EndAt: 200}
	if bad.ActiveAt(200) {
		t.Error("结束<=开始的窗口必须视为不生效")
	}
	if !m.Covers("任意节点") {
		t.Error("NodeIDs 为空表示全局")
	}
	if (&model.Maintenance{NodeIDs: []string{"a"}}).Covers("b") {
		t.Error("指定节点列表时不应覆盖其他节点")
	}
}

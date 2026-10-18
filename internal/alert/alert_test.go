package alert

import (
	"testing"
	"time"

	"github.com/motao/rollcall/internal/model"
	"github.com/motao/rollcall/internal/notify"
	"github.com/motao/rollcall/internal/store"
)

// TestDisabledRuleResolvesItsAlerts 是回归测试。
//
// 曾经的缺陷：Evaluate 对 !rule.Enabled 直接 continue，
// 于是规则停用后，它名下的活跃告警再没有任何关闭路径 ——
// 引擎的恢复分支只对启用中的规则运行，界面上会永远挂着「进行中」。
func TestDisabledRuleResolvesItsAlerts(t *testing.T) {
	st := store.New(t.TempDir())
	e := New(st)

	// 节点从未上报 + Duration=0 的失联规则 → 一轮评估即触发
	st.UpsertNode(&model.Node{ID: "n1", Name: "节点一"})
	st.UpsertRule(&model.AlertRule{
		ID: "r_off", Name: "节点失联", Metric: "offline", Op: "gt",
		Threshold: 0, Duration: 0, Severity: "critical", Enabled: true,
	})

	e.Evaluate()
	if got := st.ActiveAlertCount(); got != 1 {
		t.Fatalf("触发告警失败: ActiveAlertCount=%d, 期望 1", got)
	}

	// 停用规则
	st.UpsertRule(&model.AlertRule{
		ID: "r_off", Name: "节点失联", Metric: "offline", Op: "gt",
		Threshold: 0, Duration: 0, Severity: "critical", Enabled: false,
	})

	e.Evaluate()
	if got := st.ActiveAlertCount(); got != 0 {
		t.Errorf("停用规则后活跃告警未关闭: got %d, 期望 0", got)
	}

	// 引擎状态也应被清理，否则随规则/节点增删无限增长
	e.mu.Lock()
	lens := map[string]int{"since": len(e.since), "active": len(e.active), "lastAt": len(e.lastAt)}
	e.mu.Unlock()
	for k, v := range lens {
		if v != 0 {
			t.Errorf("引擎状态 %s 未清理: 残留 %d 条", k, v)
		}
	}
}

// TestDeletedRuleResolvesItsAlerts 验证删除规则同样不留僵尸告警。
func TestDeletedRuleResolvesItsAlerts(t *testing.T) {
	st := store.New(t.TempDir())
	e := New(st)

	st.UpsertNode(&model.Node{ID: "n1", Name: "节点一"})
	st.UpsertRule(&model.AlertRule{
		ID: "r_off", Name: "节点失联", Metric: "offline", Op: "gt",
		Duration: 0, Severity: "critical", Enabled: true,
	})
	e.Evaluate()
	if got := st.ActiveAlertCount(); got != 1 {
		t.Fatalf("前置条件失败: ActiveAlertCount=%d", got)
	}

	if !st.DeleteRule("r_off") {
		t.Fatal("DeleteRule 返回 false")
	}
	if got := st.ActiveAlertCount(); got != 0 {
		t.Errorf("删除规则后仍有活跃告警: got %d", got)
	}

	e.Evaluate() // 不应 panic，且状态应清空
	e.mu.Lock()
	n := len(e.active) + len(e.since) + len(e.lastAt)
	e.mu.Unlock()
	if n != 0 {
		t.Errorf("引擎状态残留 %d 条", n)
	}
}

// TestEngineStateDoesNotLeakOnNodeDeletion 验证节点删除后引擎状态不泄漏。
func TestEngineStateDoesNotLeakOnNodeDeletion(t *testing.T) {
	st := store.New(t.TempDir())
	e := New(st)

	st.UpsertRule(&model.AlertRule{
		ID: "r_off", Name: "节点失联", Metric: "offline", Op: "gt",
		Duration: 0, Severity: "critical", Enabled: true,
	})

	// 模拟节点反复创建又删除（40 台机群换机很常见）
	for i := 0; i < 20; i++ {
		id := "n_tmp"
		st.UpsertNode(&model.Node{ID: id, Name: "临时节点"})
		e.Evaluate()
		st.DeleteNode(id)
		// 删除节点后活跃告警应被关闭，而非永久挂起
		if got := st.ActiveAlertCount(); got != 0 {
			t.Errorf("第 %d 轮：删除节点后仍有 %d 条活跃告警", i, got)
			break
		}
		e.Evaluate()
	}

	e.mu.Lock()
	n := len(e.active) + len(e.since) + len(e.lastAt)
	e.mu.Unlock()
	if n != 0 {
		t.Errorf("20 轮增删后引擎状态泄漏 %d 条", n)
	}
}

// TestDescribeRespectsOp 验证 lt 规则不再误报「超过阈值」。
func TestDescribeRespectsOp(t *testing.T) {
	cases := []struct {
		op      string
		wantSub string
		badSub  string
	}{
		{"gt", "超过阈值", "低于阈值"},
		{"lt", "低于阈值", "超过阈值"},
	}
	for _, c := range cases {
		got := describe(
			&model.AlertRule{Metric: "cpu", Op: c.op, Threshold: 10},
			"节点一", 5.0)
		if !containsStr(got, c.wantSub) {
			t.Errorf("op=%s 文案错误: %q 不含 %q", c.op, got, c.wantSub)
		}
		if containsStr(got, c.badSub) {
			t.Errorf("op=%s 文案错误: %q 误含 %q", c.op, got, c.badSub)
		}
	}
}

// TestRestartDoesNotDuplicateActiveAlertNotification 是回归测试：
// 引擎重启后必须从持久化告警恢复 active 状态，否则首轮评估会把
// 仍在持续的告警当成新告警再通知一遍（每次重启都骚扰一次）。
func TestRestartDoesNotDuplicateActiveAlertNotification(t *testing.T) {
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "节点一"})
	st.UpsertRule(&model.AlertRule{
		ID: "r_off", Name: "节点失联", Metric: "offline", Op: "gt",
		Duration: 0, Severity: "critical", Enabled: true,
	})
	now := time.Now().Unix()
	created, ok := st.OpenAlert(&model.Alert{
		ID: "a_old", RuleID: "r_off", NodeID: "n1", NodeName: "节点一",
		RuleName: "节点失联", Severity: "critical", StartedAt: now - 300,
	})
	if !created || !ok || st.ActiveAlertCount() != 1 {
		t.Fatal("前置条件失败：未写入未恢复告警")
	}

	e := New(st) // 模拟重启：构造新引擎
	var sent []notify.Message
	e.SetNotifier(func(m notify.Message) { sent = append(sent, m) })
	e.Evaluate()
	if len(sent) != 0 {
		t.Fatalf("重启后不应重复发送进行中的告警，实际发送 %d 条", len(sent))
	}

	// 节点恢复上报后，应正常发送一条恢复通知
	st.AddMetrics("n1", model.HostInfo{Hostname: "h"}, model.Metrics{TS: time.Now().Unix()})
	e.Evaluate()
	if len(sent) != 1 || !sent[0].Resolved {
		t.Fatalf("恢复时应发送 1 条恢复通知，实际 %+v", sent)
	}
}

func containsStr(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

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
	active := st.ActiveAlerts()
	var sent []notify.Message
	e.SetNotifier(func(m notify.Message) { sent = append(sent, m) })

	// 停用规则
	st.UpsertRule(&model.AlertRule{
		ID: "r_off", Name: "节点失联", Metric: "offline", Op: "gt",
		Threshold: 0, Duration: 0, Severity: "critical", Enabled: false,
	})

	e.Evaluate()
	if got := st.ActiveAlertCount(); got != 0 {
		t.Errorf("停用规则后活跃告警未关闭: got %d, 期望 0", got)
	}
	if len(sent) != 1 || !sent[0].Resolved || sent[0].AlertID != active[0].ID {
		t.Fatalf("停用规则应补发 1 条恢复通知，实际 %+v", sent)
	}
	if !containsStr(sent[0].Body, "规则已停用") {
		t.Errorf("恢复通知未说明停用原因: %q", sent[0].Body)
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
	active := st.ActiveAlerts()
	var sent []notify.Message
	e.SetNotifier(func(m notify.Message) { sent = append(sent, m) })

	if !st.DeleteRule("r_off") {
		t.Fatal("DeleteRule 返回 false")
	}
	if got := st.ActiveAlertCount(); got != 0 {
		t.Errorf("删除规则后仍有活跃告警: got %d", got)
	}
	e.RecoverClosed(active, "规则已删除")
	if len(sent) != 1 || !sent[0].Resolved || sent[0].AlertID != active[0].ID {
		t.Fatalf("删除规则应补发 1 条恢复通知，实际 %+v", sent)
	}
	if !containsStr(sent[0].Body, "规则已删除") {
		t.Errorf("恢复通知未说明删除原因: %q", sent[0].Body)
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
	// 恢复通知必须关联持久化告警，便于与告警历史对应。
	if sent[0].AlertID != "a_old" || sent[0].RuleID != "r_off" || sent[0].NodeID != "n1" {
		t.Fatalf("恢复通知的关联字段异常: %+v", sent[0])
	}
}

// TestRuleScopeLimitsAndResolvesAlerts 验证定向规则只评估范围内节点，
// 且规则收紧/改向时，原先命中但已不在范围内的告警会被关闭。
func TestRuleScopeLimitsAndResolvesAlerts(t *testing.T) {
	st := store.New(t.TempDir())
	e := New(st)

	st.UpsertNode(&model.Node{ID: "n1", Name: "生产一", Group: "prod"})
	st.UpsertNode(&model.Node{ID: "n2", Name: "测试一", Group: "test"})
	st.UpsertNode(&model.Node{ID: "n3", Name: "无分组"})

	// 只对 prod 分组生效；n2/n3 从未上报也不应触发
	st.UpsertRule(&model.AlertRule{
		ID: "r_off", Name: "节点失联", Metric: "offline", Op: "gt",
		Duration: 0, Severity: "critical", Enabled: true, Groups: []string{"prod"},
	})
	e.Evaluate()
	active := st.ActiveAlerts()
	if len(active) != 1 || active[0].NodeID != "n1" {
		t.Fatalf("定向规则应只告警 n1，实际 %+v", active)
	}

	// 改为只针对 n2：n1 的旧告警必须关闭并补发恢复通知
	var sent []notify.Message
	e.SetNotifier(func(m notify.Message) { sent = append(sent, m) })
	st.UpsertRule(&model.AlertRule{
		ID: "r_off", Name: "节点失联", Metric: "offline", Op: "gt",
		Duration: 0, Severity: "critical", Enabled: true, NodeIDs: []string{"n2"},
	})
	e.Evaluate()
	active = st.ActiveAlerts()
	if len(active) != 1 || active[0].NodeID != "n2" {
		t.Fatalf("改向后应只剩 n2 的告警，实际 %+v", active)
	}
	var resolved []notify.Message
	for _, m := range sent {
		if m.Resolved {
			resolved = append(resolved, m)
		}
	}
	if len(resolved) != 1 || resolved[0].NodeID != "n1" || !containsStr(resolved[0].Body, "规则范围已调整") {
		t.Fatalf("规则改向应补发 n1 的恢复通知，实际 %+v", sent)
	}

	e.mu.Lock()
	stateKeys := len(e.since) + len(e.active) + len(e.lastAt)
	e.mu.Unlock()
	if stateKeys != 3 { // 仅 n2 的 since/active/lastAt
		t.Errorf("引擎状态应只保留 n2 的条目，实际 %d 条", stateKeys)
	}
}

// TestRuleApplies 覆盖节点 ID、分组与全量三种匹配方式。
func TestRuleApplies(t *testing.T) {
	cases := []struct {
		name string
		rule *model.AlertRule
		node *model.Node
		want bool
	}{
		{"未定向匹配全部", &model.AlertRule{}, &model.Node{ID: "n1", Group: "prod"}, true},
		{"节点ID命中", &model.AlertRule{NodeIDs: []string{"n1"}}, &model.Node{ID: "n1", Group: "x"}, true},
		{"节点ID未命中", &model.AlertRule{NodeIDs: []string{"n2"}}, &model.Node{ID: "n1", Group: "prod"}, false},
		{"分组命中", &model.AlertRule{Groups: []string{"prod"}}, &model.Node{ID: "n1", Group: "prod"}, true},
		{"分组未命中", &model.AlertRule{Groups: []string{"prod"}}, &model.Node{ID: "n1", Group: "test"}, false},
		{"节点无分组不被分组规则命中", &model.AlertRule{Groups: []string{"prod"}}, &model.Node{ID: "n1"}, false},
		{"ID与分组为或关系", &model.AlertRule{NodeIDs: []string{"n9"}, Groups: []string{"prod"}}, &model.Node{ID: "n1", Group: "prod"}, true},
	}
	for _, c := range cases {
		if got := ruleApplies(c.rule, c.node); got != c.want {
			t.Errorf("%s: ruleApplies = %v, 期望 %v", c.name, got, c.want)
		}
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

// TestRecoverClosedPurgesWholeRule 是回归测试。
//
// 管理路径取的是「删除前的告警快照」，快照与 DeleteRule 之间可能又有
// 同一规则的新告警写入。旧实现只删快照里那一个精确 key，
// 新写入的条目会永久留在 active/since/lastAt 里，界面挂一条永不恢复的告警。
func TestRecoverClosedPurgesWholeRule(t *testing.T) {
	st := store.New(t.TempDir())
	e := New(st)
	st.UpsertNode(&model.Node{ID: "n1", Name: "节点一"})
	st.UpsertNode(&model.Node{ID: "n2", Name: "节点二"})
	st.UpsertRule(&model.AlertRule{
		ID: "r1", Name: "节点失联", Metric: "offline", Op: "gt",
		Threshold: 0, Duration: 0, Severity: "critical", Enabled: true,
	})
	e.Evaluate()

	// 模拟「快照之后又开了一条同规则告警」
	e.mu.Lock()
	e.active["r1|n2"] = &model.Alert{ID: "a2", RuleID: "r1", NodeID: "n2"}
	e.since["r1|n2"] = time.Now().Unix()
	e.lastAt["r1|n2"] = time.Now().Unix()
	e.mu.Unlock()

	// 只传快照里的第一条，但按规则清空
	e.RecoverRuleClosed("r1", []*model.Alert{{ID: "a1", RuleID: "r1", NodeID: "n1"}}, "规则已删除")

	e.mu.Lock()
	left := len(e.active) + len(e.since) + len(e.lastAt)
	e.mu.Unlock()
	if left != 0 {
		t.Fatalf("该规则下的引擎状态未清空，残留 %d 项", left)
	}
}

// TestResolvedClearsLastAtWithoutNotifier 是回归测试。
//
// 恢复分支的清理原来写在「通知器非空」之后：engine.Start 早于 web.New
// 装通知器、或 Outbox=nil 让配置把通知器置空时，已恢复告警的 lastAt
// 永远留着。它同时还在 seen 里，轮末清理也带不走 —— 同一 key 下次越界
// 会拿一个陈旧时间做「重复提醒」判断。
func TestResolvedClearsLastAtWithoutNotifier(t *testing.T) {
	st := store.New(t.TempDir())
	e := New(st) // 不设置通知器
	key := "r1|n1"
	e.mu.Lock()
	e.lastAt[key] = time.Now().Unix()
	e.mu.Unlock()

	e.send(&model.Alert{ID: "a1", RuleID: "r1", NodeID: "n1"}, true)

	e.mu.Lock()
	_, left := e.lastAt[key]
	e.mu.Unlock()
	if left {
		t.Fatal("通知器为空时，已恢复告警的 lastAt 仍未被清理")
	}
}

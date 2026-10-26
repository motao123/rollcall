package store

import (
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/model"
)

// TestAdjustTrafficBeforeFirstReportIgnoresLifetimeCounters 覆盖手动校正时
// 节点还没上报过的场景（迁移过来先补录、Agent 还没装）。
//
// 校正只能写用量，拿不到真实计数器基线；若把 LastRx/LastTx 当成 0 就完事，
// 下一条上报会把 /proc/net/dev 的自开机累计值整段记进本期 —— 一台跑了几个月的
// 机器会立刻显示几 TB，配额告警直接误触发。
func TestAdjustTrafficBeforeFirstReportIgnoresLifetimeCounters(t *testing.T) {
	st := New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "n1", TrafficQuotaGB: 1024, TrafficMode: "sum"})
	now := time.Now().Unix()

	const wantGB = 800
	wantBytes := uint64(wantGB) * 1024 * 1024 * 1024
	if u, ok := st.AdjustTrafficUsed("n1", wantBytes); !ok || u.Used != wantBytes {
		t.Fatalf("校正后应为 %d，实际 %d（ok=%v）", wantBytes, u.Used, ok)
	}

	// 机器已累计 2.3TB，第一次上报。
	const lifetime = uint64(2300) * 1024 * 1024 * 1024
	st.AddMetrics("n1", model.HostInfo{}, model.Metrics{TS: now, NetRx: lifetime / 2, NetTx: lifetime / 2})
	if u := st.TrafficUsage("n1"); u.Used != wantBytes {
		t.Fatalf("首次上报不应把整机累计流量记进本期：期望 %d，实际 %d（多算 %d）",
			wantBytes, u.Used, u.Used-wantBytes)
	}

	// 之后才是真正的增量。
	st.AddMetrics("n1", model.HostInfo{}, model.Metrics{TS: now + 30, NetRx: lifetime/2 + 1000, NetTx: lifetime/2 + 500})
	if u := st.TrafficUsage("n1"); u.Used != wantBytes+1500 {
		t.Fatalf("建立基线后的增量应正常累计：期望 %d，实际 %d", wantBytes+1500, u.Used)
	}
}

// TestDeleteNodeRemovesOwnedServices 删除节点必须一并删掉挂在该节点下的探测目标。
//
// 留着会继续被定时探测（对外发请求），而服务类告警因「节点不存在」永远打不开，
// 形成一块不报警的盲区；界面上用户也已经找不到这些服务了。
func TestDeleteNodeRemovesOwnedServices(t *testing.T) {
	st := New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "n1"})
	st.UpsertNode(&model.Node{ID: "n2", Name: "n2"})
	if !st.CreateService(&model.Service{ID: "s1", NodeID: "n1", Name: "s1", Type: "http", Target: "https://example.com/", Interval: 60, Enabled: true}) {
		t.Fatal("创建 s1 失败")
	}
	if !st.CreateService(&model.Service{ID: "s2", NodeID: "n2", Name: "s2", Type: "http", Target: "https://example.org/", Interval: 60, Enabled: true}) {
		t.Fatal("创建 s2 失败")
	}

	if !st.DeleteNode("n1") {
		t.Fatal("删除应成功")
	}
	got := map[string]bool{}
	for _, sv := range st.Services() {
		got[sv.ID] = true
	}
	if got["s1"] {
		t.Fatal("被删节点的探测目标应一并删除")
	}
	if !got["s2"] {
		t.Fatal("其它节点的探测目标不能被误删")
	}
}

// TestTrafficCounterResetMarksIncomplete 计数器归零（Agent 所在机器重启、
// 网卡重载）会丢一段真实流量，必须标记周期不完整让运维有机会校正；
// 而时钟回拨并不丢流量，不能误标。
func TestTrafficCounterResetMarksIncomplete(t *testing.T) {
	st := New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "n1", TrafficMode: "sum"})
	now := time.Now().Unix()

	st.AddMetrics("n1", model.HostInfo{}, model.Metrics{TS: now - 60, NetRx: 500, NetTx: 500})
	st.AddMetrics("n1", model.HostInfo{}, model.Metrics{TS: now - 30, NetRx: 1500, NetTx: 1500})
	before := st.TrafficUsage("n1")
	if before.Used != 2000 || before.Incomplete {
		t.Fatalf("正常增量后应为 2000 且不完整=false，实际 %d/%v", before.Used, before.Incomplete)
	}

	// 计数器归零：两个计数器同时变小 → 那段流量确实拿不到了
	st.AddMetrics("n1", model.HostInfo{}, model.Metrics{TS: now, NetRx: 100, NetTx: 80})
	u := st.TrafficUsage("n1")
	if u.Used != before.Used {
		t.Fatalf("归零不应改动已累计用量：期望 %d，实际 %d", before.Used, u.Used)
	}
	if !u.Incomplete {
		t.Fatal("计数器归零后周期应标记不完整（面板要提示「请校正本期」）")
	}

	// 时钟回拨：读数有效、流量没丢 → 不能标不完整
	st2 := New(t.TempDir())
	st2.UpsertNode(&model.Node{ID: "n1", Name: "n1", TrafficMode: "sum"})
	st2.AddMetrics("n1", model.HostInfo{}, model.Metrics{TS: now, NetRx: 1000, NetTx: 1000})
	st2.AddMetrics("n1", model.HostInfo{}, model.Metrics{TS: now - 30, NetRx: 1200, NetTx: 1200})
	if u := st2.TrafficUsage("n1"); u.Incomplete {
		t.Fatal("时钟回拨不该标记不完整（那段流量并没有丢）")
	}
}

// TestResolveAlertDropsPendingFiringNotification 恢复时必须撤掉队列里
// 还没投递的「告警」通知，否则开启免打扰/摘要或首次投递失败退避时，
// 运维会先收到「已恢复」，随后又收到一条「告警」而再无纠正。
func TestResolveAlertDropsPendingFiringNotification(t *testing.T) {
	st := New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "n1"})

	seedRule(t, st, "r1")
	a := &model.Alert{ID: "a1", RuleID: "r1", RuleName: "CPU 高", NodeID: "n1", NodeName: "n1", Severity: "warn"}
	created, ok := st.OpenAlert(a)
	if !ok || !created {
		t.Fatalf("应成功开立告警（created=%v ok=%v）", created, ok)
	}
	alertID := a.ID

	// 队列里：一条未投递的告警、一条未投递的恢复
	firing := &model.NotifyJob{ID: "j1", AlertID: alertID, RuleID: "r1", NodeID: "n1", Title: "CPU 高", Status: model.NotifyPending}
	resolved := &model.NotifyJob{ID: "j2", AlertID: alertID, RuleID: "r1", NodeID: "n1", Title: "CPU 高", Status: model.NotifyPending, Resolved: true}
	if !st.EnqueueNotifyJob(firing) || !st.EnqueueNotifyJob(resolved) {
		t.Fatal("入队失败（任务需要 ID）")
	}

	if !st.ResolveAlert("r1", "n1") {
		t.Fatal("应能关闭告警")
	}

	var gotFiring, gotResolved string
	for _, j := range st.NotifyJobs(100) {
		switch j.AlertID {
		case alertID:
			if j.Resolved {
				gotResolved = j.Status
			} else {
				gotFiring = j.Status
			}
		}
	}
	if gotFiring != model.NotifySuppressed {
		t.Fatalf("未投递的告警通知应被撤销，实际状态 %q", gotFiring)
	}
	if gotResolved != model.NotifyPending {
		t.Fatalf("恢复通知必须保留（仍待投递），实际状态 %q", gotResolved)
	}
}

// TestResolveRuleAlertsKeepsRecoveryNotification 停用规则会压制该规则的
// 未投递告警，但不能连「已恢复」一起压掉 —— 那条消息正是停用动作的收尾说明。
func TestResolveRuleAlertsKeepsRecoveryNotification(t *testing.T) {
	st := New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "n1"})

	seedRule(t, st, "r1")
	a := &model.Alert{ID: "a1", RuleID: "r1", RuleName: "CPU 高", NodeID: "n1", NodeName: "n1", Severity: "warn"}
	st.OpenAlert(a)

	firing := &model.NotifyJob{ID: "j1", AlertID: a.ID, RuleID: "r1", NodeID: "n1", Title: "CPU 高", Status: model.NotifyPending}
	recovery := &model.NotifyJob{ID: "j2", AlertID: a.ID, RuleID: "r1", NodeID: "n1", Title: "CPU 高", Status: model.NotifyPending, Resolved: true}
	if !st.EnqueueNotifyJob(firing) || !st.EnqueueNotifyJob(recovery) {
		t.Fatal("入队失败（任务需要 ID）")
	}

	if closed := st.ResolveRuleAlerts("r1"); len(closed) != 1 {
		t.Fatalf("应关闭 1 条告警，实际 %d", len(closed))
	}

	var gotFiring, gotRecovery string
	for _, j := range st.NotifyJobs(100) {
		if j.RuleID != "r1" {
			continue
		}
		if j.Resolved {
			gotRecovery = j.Status
		} else {
			gotFiring = j.Status
		}
	}
	if gotFiring != model.NotifySuppressed {
		t.Fatalf("未投递的告警通知应被压制，实际 %q", gotFiring)
	}
	if gotRecovery != model.NotifyPending {
		t.Fatalf("「已恢复」通知不能被压制，实际 %q", gotRecovery)
	}
}

// seedRule 建一条启用中的规则：OpenAlert 要求规则存在，否则直接返回 false。
func seedRule(t *testing.T, st *Store, id string) {
	t.Helper()
	st.UpsertRule(&model.AlertRule{ID: id, Name: "CPU 高", Metric: "cpu", Op: "gt", Threshold: 90, Severity: "warn", Enabled: true})
	found := false
	for _, r := range st.Rules() {
		if r.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("创建规则 %s 失败", id)
	}
}

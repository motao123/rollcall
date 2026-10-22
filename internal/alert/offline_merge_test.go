package alert

import (
	"strings"
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/notify"
	"github.com/motao123/rollcall/internal/store"
)

// TestOfflineNotificationsMerged 覆盖"同一轮里多台离线合并成一条"。
//
// 原来的行为是每台一条，机房整体掉线时能连刷好几分钟（配合 30 秒
// 最多 8 条的限制，用户要等很久才能看完）。单台必须保持原样，
// 否则"哪台断了"这条最关键的信息会退化成一句汇总。
func TestOfflineNotificationsMerged(t *testing.T) {
	st := store.New(t.TempDir())
	e := New(st)
	var sent []notify.Message
	e.SetNotifier(func(m notify.Message) { sent = append(sent, m) })

	rule := model.AlertRule{
		ID: "r_off", Name: "节点失联", Metric: "offline", Op: "gt",
		Threshold: 0, Duration: 0, Severity: "critical", Enabled: true,
	}
	st.UpsertRule(&rule)

	t.Run("单台不合并", func(t *testing.T) {
		sent = sent[:0]
		st.UpsertNode(&model.Node{ID: "solo", Name: "单台机"})
		e.Evaluate()
		if len(sent) != 1 {
			t.Fatalf("期望 1 条通知，实际 %d: %+v", len(sent), sent)
		}
		if sent[0].Title != "节点失联" || sent[0].NodeName != "单台机" || sent[0].AlertID == "" {
			t.Fatalf("单台通知应保持原样（标题=规则名、带节点名与告警 ID），实际 %+v", sent[0])
		}
	})

	t.Run("多台合并成一条", func(t *testing.T) {
		sent = sent[:0]
		st.UpsertNode(&model.Node{ID: "a", Name: "机-A"})
		st.UpsertNode(&model.Node{ID: "b", Name: "机-B"})
		st.UpsertNode(&model.Node{ID: "c", Name: "机-C"})
		e.Evaluate()

		offlineMsgs := 0
		var merged *notify.Message
		for i := range sent {
			if strings.Contains(sent[i].Title, "台节点失联") {
				offlineMsgs++
				merged = &sent[i]
			}
		}
		if offlineMsgs != 1 {
			t.Fatalf("期望 1 条合并通知，实际 %d 条: %+v", offlineMsgs, sent)
		}
		// 上一轮那台已处于活跃告警，本轮只新增 a/b/c 三条 —— 合并的正是这三条。
		if !strings.Contains(merged.Title, "3 台") {
			t.Fatalf("合并标题应包含本轮台数，实际 %q", merged.Title)
		}
		for _, name := range []string{"机-A", "机-B", "机-C"} {
			if !strings.Contains(merged.Body, name) {
				t.Fatalf("合并正文缺少 %s: %q", name, merged.Body)
			}
		}
		// 合并消息不绑定单台节点，也不带单个 AlertID
		if merged.NodeID != "" || merged.AlertID != "" {
			t.Fatalf("合并通知不应绑定单个节点: %+v", merged)
		}
		if merged.RuleID != "r_off" {
			t.Fatalf("同一规则下应保留 RuleID（静默才好生效），实际 %q", merged.RuleID)
		}
	})
}

// TestOfflineBatchFlushesOnSingleEvaluation 确认批次不会跨轮次累积：
// 第二轮只评估到"没有新增离线"时不应再补发旧消息。
func TestOfflineBatchFlushesOnSingleEvaluation(t *testing.T) {
	st := store.New(t.TempDir())
	e := New(st)
	var sent []notify.Message
	e.SetNotifier(func(m notify.Message) { sent = append(sent, m) })

	st.UpsertRule(&model.AlertRule{
		ID: "r_off", Name: "节点失联", Metric: "offline", Op: "gt",
		Threshold: 0, Duration: 0, Severity: "critical", Enabled: true,
	})
	st.UpsertNode(&model.Node{ID: "n1", Name: "机-1"})
	e.Evaluate()
	first := len(sent)
	if first != 1 {
		t.Fatalf("首轮应发 1 条，实际 %d", first)
	}
	// 第二轮：节点仍离线，但告警已在活跃态、未到重复提醒间隔 → 不应再发
	e.Evaluate()
	if len(sent) != first {
		t.Fatalf("第二轮不应重复发送，实际多出 %d 条", len(sent)-first)
	}
}

// TestOfflineRecoveryMerged 覆盖"同一轮里多台恢复合并成一条"。
// 生产上删除节点不会补发恢复（节点的告警随节点一起消失），
// 所以这条路径只能靠单测覆盖：三台同时重新上报，应当只收到一条汇总。
func TestOfflineRecoveryMerged(t *testing.T) {
	st := store.New(t.TempDir())
	e := New(st)
	var sent []notify.Message
	e.SetNotifier(func(m notify.Message) { sent = append(sent, m) })

	st.UpsertRule(&model.AlertRule{
		ID: "r_off", Name: "节点失联", Metric: "offline", Op: "gt",
		Threshold: 0, Duration: 0, Severity: "critical", Enabled: true,
	})
	for _, n := range []struct{ id, name string }{{"a", "机-A"}, {"b", "机-B"}, {"c", "机-C"}} {
		st.UpsertNode(&model.Node{ID: n.id, Name: n.name})
	}
	e.Evaluate() // 三台同时离线 → 一条合并的"失联"

	sent = sent[:0]
	now := time.Now().Unix()
	for _, id := range []string{"a", "b", "c"} {
		st.AddMetrics(id, model.HostInfo{}, model.Metrics{TS: now, CPUPct: 5})
	}
	e.Evaluate() // 三台同时恢复 → 一条合并的"恢复"

	var recovered *notify.Message
	for i := range sent {
		if strings.Contains(sent[i].Title, "台节点恢复") {
			recovered = &sent[i]
			break
		}
	}
	if recovered == nil {
		t.Fatalf("应有一条合并的恢复通知，实际 %+v", sent)
	}
	if !recovered.Resolved {
		t.Fatal("恢复通知的 Resolved 必须为 true（免打扰/摘要在恢复时不生效）")
	}
	for _, name := range []string{"机-A", "机-B", "机-C"} {
		if !strings.Contains(recovered.Body, name) {
			t.Fatalf("恢复正文缺少 %s: %q", name, recovered.Body)
		}
	}
}

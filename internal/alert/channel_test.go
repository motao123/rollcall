package alert

import (
	"testing"

	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/notify"
	"github.com/motao123/rollcall/internal/store"
)

// TestAlertCarriesRuleChannels 是 v1.34.0 的端到端用例：
// 规则上的渠道选择必须一路传到通知消息，否则前面两段（notify 过滤、接口校验）
// 各自都正确，整体却"配了不生效"—— 而且不会报错，最难发现。
func TestAlertCarriesRuleChannels(t *testing.T) {
	st := store.New(t.TempDir())
	e := New(st)
	var sent []notify.Message
	e.SetNotifier(func(m notify.Message) { sent = append(sent, m) })

	st.UpsertNode(&model.Node{ID: "n1", Name: "节点一"})
	st.UpsertRule(&model.AlertRule{
		ID: "r_off", Name: "节点失联", Metric: "offline", Op: "gt",
		Threshold: 0, Duration: 0, Severity: "critical", Enabled: true,
		Channels: []string{"ch_ops", "ch_audit"},
	})

	e.Evaluate()
	if len(sent) != 1 {
		t.Fatalf("应发出 1 条通知，实际 %d", len(sent))
	}
	got := sent[0].ChannelIDs
	if len(got) != 2 || got[0] != "ch_ops" || got[1] != "ch_audit" {
		t.Fatalf("通知消息应带规则指定的渠道，实际 %v", got)
	}
}

// TestAlertChannelsFollowRuleChanges 确认恢复通知用的是**当前**规则配置。
//
// 用户在告警存续期间把渠道改到值班群，说明"这条以后发这里"；
// 恢复通知仍发到旧渠道会让人以为配置没生效。
func TestAlertChannelsFollowRuleChanges(t *testing.T) {
	st := store.New(t.TempDir())
	e := New(st)
	var sent []notify.Message
	e.SetNotifier(func(m notify.Message) { sent = append(sent, m) })

	st.UpsertNode(&model.Node{ID: "n1", Name: "节点一"})
	st.UpsertRule(&model.AlertRule{
		ID: "r_off", Name: "节点失联", Metric: "offline", Op: "gt",
		Threshold: 0, Duration: 0, Severity: "critical", Enabled: true,
		Channels: []string{"ch_old"},
	})
	e.Evaluate()

	// 改渠道后停用规则 → 触发恢复通知
	st.UpsertRule(&model.AlertRule{
		ID: "r_off", Name: "节点失联", Metric: "offline", Op: "gt",
		Threshold: 0, Duration: 0, Severity: "critical", Enabled: false,
		Channels: []string{"ch_new"},
	})
	e.Evaluate()

	if len(sent) != 2 {
		t.Fatalf("应有告警 + 恢复 2 条，实际 %d", len(sent))
	}
	res := sent[1]
	if !res.Resolved {
		t.Fatalf("第二条应是恢复通知，实际 %+v", res)
	}
	if len(res.ChannelIDs) != 1 || res.ChannelIDs[0] != "ch_new" {
		t.Fatalf("恢复通知应使用当前规则渠道 ch_new，实际 %v", res.ChannelIDs)
	}
}

// TestAlertWithoutChannelsSendsToAll 确认默认行为没变（留空=全部渠道）。
func TestAlertWithoutChannelsSendsToAll(t *testing.T) {
	st := store.New(t.TempDir())
	e := New(st)
	var sent []notify.Message
	e.SetNotifier(func(m notify.Message) { sent = append(sent, m) })

	st.UpsertNode(&model.Node{ID: "n1", Name: "节点一"})
	st.UpsertRule(&model.AlertRule{
		ID: "r_off", Name: "节点失联", Metric: "offline", Op: "gt",
		Threshold: 0, Duration: 0, Severity: "critical", Enabled: true,
	})
	e.Evaluate()
	if len(sent) != 1 {
		t.Fatalf("应发出 1 条通知，实际 %d", len(sent))
	}
	if len(sent[0].ChannelIDs) != 0 {
		t.Fatalf("未限定渠道时应为空（=全部），实际 %v", sent[0].ChannelIDs)
	}
}

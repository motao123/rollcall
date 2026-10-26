package alert

import (
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/notify"
	"github.com/motao123/rollcall/internal/store"
)

// seedTempRule 建一条温度规则；阈值 80℃。
func seedTempRule(t *testing.T, st *store.Store, threshold float64) {
	t.Helper()
	st.UpsertRule(&model.AlertRule{
		ID: "r_temp", Name: "温度过高", Metric: "temp", Op: "gt",
		Threshold: threshold, Duration: 0, Severity: "warn", Enabled: true,
	})
}

// TestTempRuleFiresAndRespectsNoSensor 覆盖温度告警的两面：
// 有读数且越界要触发；没有传感器（TempC=0）必须按"无数据"处理 ——
// 既不能触发，也不能把 0℃ 当成一次恢复。
func TestTempRuleFiresAndRespectsNoSensor(t *testing.T) {
	st := store.New(t.TempDir())
	e := New(st)
	var sent []notify.Message
	e.SetNotifier(func(m notify.Message) { sent = append(sent, m) })

	st.UpsertNode(&model.Node{ID: "n1", Name: "带传感器"})
	seedTempRule(t, st, 80)

	now := time.Now().Unix()
	st.AddMetrics("n1", model.HostInfo{}, model.Metrics{TS: now, TempC: 85.5})
	e.Evaluate()
	if got := st.ActiveAlertCount(); got != 1 {
		t.Fatalf("85.5℃ 越界 80℃ 应触发告警，实际 %d 条", got)
	}
	if len(sent) != 1 || sent[0].RuleID != "r_temp" {
		t.Fatalf("应发出 1 条告警通知，实际 %+v", sent)
	}
	if !containsStr(sent[0].Body, "85.5") {
		t.Errorf("告警文案应包含读数，实际 %q", sent[0].Body)
	}

	// 温度回落（未越界但仍有读数）→ 恢复
	st.AddMetrics("n1", model.HostInfo{}, model.Metrics{TS: now + 30, TempC: 62})
	e.Evaluate()
	if got := st.ActiveAlertCount(); got != 0 {
		t.Fatalf("温度回落后应恢复，实际 %d 条活跃", got)
	}

	// 节点换成了没有传感器的机器（TempC=0）：按"无数据"处理，
	// 不能重新触发，也不能当作一次新的恢复反复通知。
	st.AddMetrics("n1", model.HostInfo{}, model.Metrics{TS: now + 60})
	e.Evaluate()
	if got := st.ActiveAlertCount(); got != 0 {
		t.Fatalf("无传感器时不应有活跃告警，实际 %d 条", got)
	}
	if n := len(sent); n != 2 {
		t.Fatalf("无传感器阶段不应再发通知（实际 %d 条）", n)
	}
}

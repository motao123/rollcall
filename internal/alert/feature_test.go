package alert

import (
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/store"
)

func TestAlertKeySeparatesServices(t *testing.T) {
	a := alertKey(&model.Alert{RuleID: "r1", NodeID: "n1", ServiceID: "s1"})
	b := alertKey(&model.Alert{RuleID: "r1", NodeID: "n1", ServiceID: "s2"})
	c := alertKey(&model.Alert{RuleID: "r1", NodeID: "n1"})
	if a == b || a == c || b == c {
		t.Fatalf("规则/节点相同但服务不同时键必须不同: %q %q %q", a, b, c)
	}
	if !isServiceMetric("service_down") || !isServiceMetric("cert_days") ||
		!isServiceMetric("latency") || !isServiceMetric("loss") || !isServiceMetric("jitter") {
		t.Error("服务类指标识别不完整")
	}
	if isServiceMetric("cpu") || isServiceMetric("traffic_quota") {
		t.Error("节点/流量指标不应被识别为服务指标")
	}
}

// TestServiceDownScopedToSelectedService 验证 ServiceIDs 只对指定服务生效。
func TestServiceDownScopedToSelectedService(t *testing.T) {
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "节点一"})
	st.UpsertService(&model.Service{ID: "s1", NodeID: "n1", Name: "Web", Type: "http", Target: "http://127.0.0.1", Enabled: true})
	st.UpsertService(&model.Service{ID: "s2", NodeID: "n1", Name: "API", Type: "http", Target: "http://127.0.0.1", Enabled: true})
	st.UpsertRule(&model.AlertRule{
		ID: "r1", Name: "服务故障", Metric: "service_down", Op: "gt",
		Threshold: 1, Duration: 0, Severity: "critical", Enabled: true,
		ServiceIDs: []string{"s1"},
	})

	e := New(st)
	e.Evaluate()

	active := st.ActiveAlerts()
	if len(active) != 1 {
		t.Fatalf("应只对范围内的 s1 告警，实际 %d 条: %+v", len(active), active)
	}
	if active[0].ServiceID != "s1" || active[0].ServiceName != "Web" {
		t.Errorf("告警关联服务错误: %+v", active[0])
	}
}

// TestCheckServiceNoDataOnlyDownTriggers 回归：从未探测的服务不能因为
// 延迟/证书规则就误报；service_down 才把「没有结果」视为故障。
func TestCheckServiceNoDataOnlyDownTriggers(t *testing.T) {
	st := store.New(t.TempDir())
	e := New(st)
	now := time.Now().Unix()

	if _, breached, known := e.checkService(&model.AlertRule{Metric: "service_down", Op: "gt", Threshold: 1}, "missing", now); !known || !breached {
		t.Errorf("无探测结果时 service_down 应视为故障: known=%v breached=%v", known, breached)
	}
	for _, metric := range []string{"latency", "cert_days", "loss", "jitter"} {
		if _, breached, known := e.checkService(&model.AlertRule{Metric: metric, Op: "gt", Threshold: 1}, "missing", now); known || breached {
			t.Errorf("%s 在无数据时不应产生告警: known=%v breached=%v", metric, known, breached)
		}
	}
}

func TestTrafficQuotaRuleTriggers(t *testing.T) {
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "节点一", Token: "t", TrafficQuotaGB: 1, TrafficResetDay: 1})
	now := time.Now().Unix()
	// 首点建立基线，随后累计 2GB（上下行各 1GB）。
	st.AddMetrics("n1", model.HostInfo{}, model.Metrics{TS: now - 120, NetRx: 0, NetTx: 0})
	st.AddMetrics("n1", model.HostInfo{}, model.Metrics{TS: now - 60, NetRx: 1 << 30, NetTx: 1 << 30})
	if used := st.TrafficUsage("n1"); used.UsedRx+used.UsedTx != 2<<30 {
		t.Fatalf("流量累计前置条件失败: %+v", used)
	}
	st.UpsertRule(&model.AlertRule{
		ID: "rq", Name: "流量配额", Metric: "traffic_quota", Op: "gt",
		Threshold: 90, Duration: 0, Severity: "warn", Enabled: true,
	})

	e := New(st)
	e.Evaluate()
	active := st.ActiveAlerts()
	if len(active) != 1 || active[0].RuleID != "rq" {
		t.Fatalf("流量超过配额应触发告警: %+v", active)
	}
	if active[0].Value < 190 || active[0].Value > 210 {
		t.Errorf("告警值应为约 200%%，实际 %.1f", active[0].Value)
	}
}

// TestTrafficQuotaHonorsLtOp 回归：流量配额同样支持 lt（例如"用量异常偏低"
// 用于发现计数器重置/线路中断），判定必须按 rule.Op 而不是固定 >=。
func TestTrafficQuotaHonorsLtOp(t *testing.T) {
	newStore := func(op string, threshold float64) *store.Store {
		st := store.New(t.TempDir())
		st.UpsertNode(&model.Node{ID: "n1", Name: "节点一", Token: "t", TrafficQuotaGB: 10, TrafficResetDay: 1})
		now := time.Now().Unix()
		// 10GB 配额用了 2GB → 20%
		st.AddMetrics("n1", model.HostInfo{}, model.Metrics{TS: now - 120, NetRx: 0, NetTx: 0})
		st.AddMetrics("n1", model.HostInfo{}, model.Metrics{TS: now - 60, NetRx: 1 << 30, NetTx: 1 << 30})
		st.UpsertRule(&model.AlertRule{
			ID: "rq", Name: "流量配额", Metric: "traffic_quota", Op: op,
			Threshold: threshold, Duration: 0, Severity: "warn", Enabled: true,
		})
		return st
	}

	lt := newStore("lt", 25) // 20% < 25%
	New(lt).Evaluate()
	active := lt.ActiveAlerts()
	if len(active) != 1 || active[0].RuleID != "rq" {
		t.Fatalf("lt 规则应在用量低于阈值时触发: %+v", active)
	}
	if active[0].Value < 19 || active[0].Value > 21 {
		t.Errorf("告警值应约为 20%%，实际 %.1f", active[0].Value)
	}

	gt := newStore("gt", 25) // 20% 不低于 25%
	New(gt).Evaluate()
	if got := gt.ActiveAlertCount(); got != 0 {
		t.Errorf("gt 规则在用量低于阈值时不应触发，实际 %d 条", got)
	}
}

// TestTrafficQuotaExactThresholdDoesNotExceed 回归：恰好等于阈值不算"超过"，
// 与 CPU/内存等指标保持同一语义。
func TestTrafficQuotaExactThresholdDoesNotExceed(t *testing.T) {
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "节点一", Token: "t", TrafficQuotaGB: 2, TrafficResetDay: 1})
	now := time.Now().Unix()
	st.AddMetrics("n1", model.HostInfo{}, model.Metrics{TS: now - 120, NetRx: 0, NetTx: 0})
	st.AddMetrics("n1", model.HostInfo{}, model.Metrics{TS: now - 60, NetRx: 1 << 30, NetTx: 1 << 30})
	st.UpsertRule(&model.AlertRule{
		ID: "rq", Name: "流量配额", Metric: "traffic_quota", Op: "gt",
		Threshold: 100, Duration: 0, Severity: "warn", Enabled: true,
	})
	New(st).Evaluate()
	if got := st.ActiveAlertCount(); got != 0 {
		t.Errorf("用量恰好 100%% 不应触发 gt 规则，实际 %d 条", got)
	}
}

func TestQuotaUnsetDoesNotTrigger(t *testing.T) {
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "节点一", Token: "t"})
	st.UpsertRule(&model.AlertRule{
		ID: "rq", Name: "流量配额", Metric: "traffic_quota", Op: "gt",
		Threshold: 1, Duration: 0, Severity: "warn", Enabled: true,
	})
	e := New(st)
	e.Evaluate()
	if got := st.ActiveAlertCount(); got != 0 {
		t.Errorf("未设置配额时不应告警，实际 %d 条", got)
	}
}

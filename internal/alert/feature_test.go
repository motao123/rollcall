package alert

import (
	"testing"
	"time"

	"github.com/motao/rollcall/internal/model"
	"github.com/motao/rollcall/internal/store"
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

package store

import (
	"math"
	"testing"
	"time"

	"github.com/motao/rollcall/internal/model"
)

func drainSave(st *Store) {
	select {
	case <-st.saveReq:
	default:
	}
}

func waitSave(t *testing.T, st *Store, what string) {
	t.Helper()
	select {
	case <-st.saveReq:
	case <-time.After(time.Second):
		t.Fatalf("%s 未请求落盘", what)
	}
}

// TestAddMetricsUpdatesHostOnDuplicateTimestamp 是回归测试。
//
// 同一秒内重复上报会走「覆盖原始点」的早退分支，原来的 return 在
// 更新 hosts 之前，于是这期间变化的 hostname / 内核版本永远写不进去。
func TestAddMetricsUpdatesHostOnDuplicateTimestamp(t *testing.T) {
	st := New(t.TempDir())
	ts := time.Now().Unix()
	st.AddMetrics("n1", model.HostInfo{Hostname: "old"}, model.Metrics{TS: ts})
	st.AddMetrics("n1", model.HostInfo{Hostname: "new"}, model.Metrics{TS: ts})

	if got := st.Host("n1").Hostname; got != "new" {
		t.Fatalf("重复时间戳的样本也应更新主机信息，实际 %q", got)
	}
}

// TestAlertMutationsRequestSave 是回归测试。
//
// 告警记录是持久化数据。打开/恢复只改内存不请求落盘时，重启后
// 「进行中」的告警全部消失、已恢复的告警「复活」——而引擎只对启用中的
// 规则跑恢复分支，会留下永不自愈的僵尸告警。
func TestAlertMutationsRequestSave(t *testing.T) {
	st := New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "n1"})
	st.UpsertRule(&model.AlertRule{ID: "r1", Name: "规则", Metric: "cpu", Op: "gt", Threshold: 90, Enabled: true})
	drainSave(st)

	created, ok := st.OpenAlert(&model.Alert{ID: "a1", RuleID: "r1", NodeID: "n1", StartedAt: time.Now().Unix()})
	if !created || !ok {
		t.Fatalf("应新建告警: created=%v ok=%v", created, ok)
	}
	waitSave(t, st, "OpenAlert")

	// 同一规则/节点再次打开：走「就地更新值」分支，同样必须落盘
	drainSave(st)
	st.OpenAlert(&model.Alert{RuleID: "r1", NodeID: "n1", StartedAt: time.Now().Unix()})
	waitSave(t, st, "OpenAlert（就地更新）")

	drainSave(st)
	if !st.ResolveAlert("r1", "n1") {
		t.Fatal("应关闭告警")
	}
	waitSave(t, st, "ResolveAlert")

	// 再开一条，验证按规则批量关闭也落盘
	st.OpenAlert(&model.Alert{RuleID: "r1", NodeID: "n1", StartedAt: time.Now().Unix()})
	drainSave(st)
	if got := st.ResolveRuleAlerts("r1"); len(got) != 1 {
		t.Fatalf("应关闭 1 条，实际 %d", len(got))
	}
	waitSave(t, st, "ResolveRuleAlerts")
}

// TestSetSettingRequestsSave 是回归测试。
// 主题、通知开关等设置只写内存时，保存后立刻重启会静默回退。
func TestSetSettingRequestsSave(t *testing.T) {
	st := New(t.TempDir())
	drainSave(st)
	st.SetSetting("theme", `{"mode":"dark"}`)
	waitSave(t, st, "SetSetting")

	// 值未变化时不必重复落盘
	drainSave(st)
	st.SetSetting("theme", `{"mode":"dark"}`)
	select {
	case <-st.saveReq:
		t.Fatal("相同值不应触发落盘")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestSanitizeBucketClampsPersistedValues 是回归测试。
//
// history.json 的 Count 是增量平均的分母，也为回填绝对用量总和提供乘数；
// NaN/Inf 百分比会让整份快照永远无法编码。
func TestSanitizeBucketClampsPersistedValues(t *testing.T) {
	b := sanitizeBucket(historyBucket{
		Index: 1, Count: 5_000_000,
		CPUAvg: float32(math.NaN()), CPUMax: 150,
		MemAvg: -1, DiskAvg: 99,
		Load1Avg: float32(math.Inf(1)),
	})
	if b.Count != maxBucketCount {
		t.Errorf("Count 未夹取: %d", b.Count)
	}
	if b.CPUAvg != 0 || b.CPUMax != 100 || b.MemAvg != 0 || b.DiskAvg != 99 {
		t.Errorf("百分比未被夹取到 0–100: %+v", b)
	}
	if b.Load1Avg != 0 {
		t.Errorf("Inf 负载未归零: %v", b.Load1Avg)
	}
}

package outbox

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/motao/rollcall/internal/model"
	"github.com/motao/rollcall/internal/notify"
	"github.com/motao/rollcall/internal/store"
)

func testNotifyConfig() *notify.Config {
	return &notify.Config{
		Enabled:     true,
		MinSeverity: "info",
		Channels: []notify.ChannelConfig{{
			ID: "c1", Type: "webhook", Name: "test", Enabled: true,
			Options: map[string]string{"url": "http://127.0.0.1/hook"},
		}},
	}
}

func testManager(st *store.Store) *Manager {
	return New(st, Config{
		Workers:     2,
		Tick:        10 * time.Millisecond,
		MaxAttempts: 3,
		Backoff:     []time.Duration{20 * time.Millisecond, 40 * time.Millisecond},
	})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("等待超时: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func msg() notify.Message {
	return notify.Message{
		Title: "CPU 告警", Body: "详情", Severity: "critical",
		NodeID: "n1", NodeName: "节点一", RuleID: "r_cpu", RuleName: "CPU",
		AlertID: "al_1", TS: time.Now().Unix(),
	}
}

func TestDeliversAndRecordsSuccess(t *testing.T) {
	st := store.New(t.TempDir())
	m := testManager(st)
	var calls atomic.Int32
	m.send = func(*notify.Config, notify.Message) []notify.Result {
		calls.Add(1)
		return []notify.Result{{Channel: "test", OK: true}}
	}
	m.SetConfig(testNotifyConfig())
	m.Start()
	defer m.Stop()

	m.Enqueue(msg())
	waitFor(t, "送达", func() bool { return st.NotifyStats().Sent == 1 })
	s := st.NotifyStats()
	if s.Pending != 0 || s.Failed != 0 || s.LastSuccess == 0 {
		t.Fatalf("统计异常: %+v", s)
	}
	if calls.Load() != 1 {
		t.Fatalf("应只投递一次，实际 %d", calls.Load())
	}
}

func TestSilenceSuppressesWithoutSending(t *testing.T) {
	st := store.New(t.TempDir())
	st.SetSilence(&model.Silence{Scope: "global", Until: time.Now().Unix() + 600, Reason: "维护"})
	m := testManager(st)
	var calls atomic.Int32
	m.send = func(*notify.Config, notify.Message) []notify.Result {
		calls.Add(1)
		return []notify.Result{{Channel: "test", OK: true}}
	}
	m.SetConfig(testNotifyConfig())
	m.Start()
	defer m.Stop()

	m.Enqueue(msg())
	waitFor(t, "静默计数", func() bool { return st.NotifyStats().Suppressed == 1 })
	if calls.Load() != 0 {
		t.Fatalf("静默期间不应外发，实际调用 %d 次", calls.Load())
	}
	if s := st.NotifyStats(); s.Pending != 0 || s.Sent != 0 {
		t.Fatalf("统计异常: %+v", s)
	}
}

func TestRetriesWithBackoffThenSucceeds(t *testing.T) {
	st := store.New(t.TempDir())
	m := testManager(st)
	var calls atomic.Int32
	m.send = func(*notify.Config, notify.Message) []notify.Result {
		if calls.Add(1) < 3 {
			return []notify.Result{{Channel: "test", OK: false, Error: "connection refused"}}
		}
		return []notify.Result{{Channel: "test", OK: true}}
	}
	m.SetConfig(testNotifyConfig())
	m.Start()
	defer m.Stop()

	m.Enqueue(msg())
	waitFor(t, "重试后送达", func() bool { return st.NotifyStats().Sent == 1 })
	s := st.NotifyStats()
	if s.Retries != 2 || s.Failed != 0 {
		t.Fatalf("统计异常: %+v", s)
	}
}

func TestExhaustedRetriesMarkFailed(t *testing.T) {
	st := store.New(t.TempDir())
	m := testManager(st)
	var calls atomic.Int32
	m.send = func(*notify.Config, notify.Message) []notify.Result {
		calls.Add(1)
		return []notify.Result{{Channel: "test", OK: false, Error: "502"}}
	}
	m.SetConfig(testNotifyConfig())
	m.Start()
	defer m.Stop()

	m.Enqueue(msg())
	waitFor(t, "最终失败", func() bool { return st.NotifyStats().Failed == 1 })
	if calls.Load() != 3 {
		t.Fatalf("应尝试 3 次，实际 %d", calls.Load())
	}
	if s := st.NotifyStats(); s.Pending != 0 || s.Retries != 2 {
		t.Fatalf("统计异常: %+v", s)
	}
}

func TestRestartRecoversPendingJobs(t *testing.T) {
	dir := t.TempDir()
	st := store.New(dir)
	now := time.Now().Unix()
	if !st.EnqueueNotifyJob(&model.NotifyJob{
		ID: "nj_restart", RuleID: "r_cpu", Severity: "critical", Title: "重启恢复",
		CreatedAt: now, NextAttemptAt: now, Status: model.NotifyPending,
	}) {
		t.Fatal("入队失败")
	}
	if err := st.Save(); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	re := store.New(dir)
	m := testManager(re)
	var got atomic.Int32
	m.send = func(_ *notify.Config, m notify.Message) []notify.Result {
		if m.Title != "重启恢复" {
			t.Errorf("恢复的任务标题异常: %s", m.Title)
		}
		got.Add(1)
		return []notify.Result{{Channel: "test", OK: true}}
	}
	m.SetConfig(testNotifyConfig())
	m.Start()
	defer m.Stop()

	waitFor(t, "重启任务送达", func() bool { return re.NotifyStats().Sent == 1 })
	if got.Load() != 1 {
		t.Fatalf("应投递 1 次，实际 %d", got.Load())
	}
}

func TestWorkerConcurrencyIsBounded(t *testing.T) {
	st := store.New(t.TempDir())
	m := testManager(st)
	m.cfg.Workers = 2
	release := make(chan struct{})
	var inFlight, peak atomic.Int32
	m.send = func(*notify.Config, notify.Message) []notify.Result {
		cur := inFlight.Add(1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		<-release
		inFlight.Add(-1)
		return []notify.Result{{Channel: "test", OK: true}}
	}
	m.SetConfig(testNotifyConfig())
	m.Start()
	defer m.Stop()

	for i := 0; i < 5; i++ {
		msg := msg()
		msg.AlertID = "al_" + string(rune('a'+i))
		m.Enqueue(msg)
	}
	waitFor(t, "两个任务在途", func() bool { return inFlight.Load() == 2 })
	time.Sleep(50 * time.Millisecond)
	if peak.Load() > 2 {
		t.Fatalf("并发峰值 %d，超过 worker 上限 2", peak.Load())
	}
	close(release)
	waitFor(t, "全部送达", func() bool { return st.NotifyStats().Sent == 5 })
}

func TestDisabledConfigAndSeverityFilterDropAtEnqueue(t *testing.T) {
	st := store.New(t.TempDir())
	m := testManager(st)
	m.send = func(*notify.Config, notify.Message) []notify.Result {
		t.Fatal("不应调用投递")
		return nil
	}
	cfg := testNotifyConfig()
	cfg.MinSeverity = "critical"
	m.SetConfig(cfg)

	warn := msg()
	warn.Severity = "warn"
	m.Enqueue(warn)
	if s := st.NotifyStats(); s.Pending != 0 {
		t.Fatalf("严重度不足的通知不应入队: %+v", s)
	}

	off := testNotifyConfig()
	off.Enabled = false
	m.SetConfig(off)
	m.Enqueue(msg())
	if s := st.NotifyStats(); s.Pending != 0 {
		t.Fatalf("通知停用后不应入队: %+v", s)
	}
}

func TestSilenceAppliedToQueuedJob(t *testing.T) {
	st := store.New(t.TempDir())
	m := testManager(st)
	var calls atomic.Int32
	m.send = func(*notify.Config, notify.Message) []notify.Result {
		calls.Add(1)
		return []notify.Result{{Channel: "test", OK: true}}
	}
	cfg := testNotifyConfig()
	m.SetConfig(cfg)
	// 入队时无静默，投递前进入静默：执行阶段仍应拦截。
	now := time.Now().Unix()
	st.EnqueueNotifyJob(&model.NotifyJob{
		ID: "nj_queued", RuleID: "r_cpu", Severity: "critical", Title: "已排队",
		CreatedAt: now, NextAttemptAt: now, Status: model.NotifyPending,
	})
	st.SetSilence(&model.Silence{Scope: "r_cpu", Until: now + 600})
	m.Start()
	defer m.Stop()

	waitFor(t, "静默拦截", func() bool { return st.NotifyStats().Suppressed == 1 })
	if calls.Load() != 0 {
		t.Fatalf("执行阶段应再次检查静默，实际投递 %d 次", calls.Load())
	}
}

func TestResultSummaryPartialSuccess(t *testing.T) {
	ok, errs := resultSummary([]notify.Result{
		{Channel: "a", OK: true},
		{Channel: "b", OK: false, Error: "boom"},
	})
	if !ok || errs == "" {
		t.Fatalf("部分成功应视为送达并保留错误: ok=%v errs=%q", ok, errs)
	}
	ok, errs = resultSummary([]notify.Result{{Channel: "a", OK: false, Error: "x"}})
	if ok || errs == "" {
		t.Fatalf("全部失败应返回失败: ok=%v errs=%q", ok, errs)
	}
}

func TestStopIsIdempotent(t *testing.T) {
	st := store.New(t.TempDir())
	m := testManager(st)
	m.send = func(*notify.Config, notify.Message) []notify.Result {
		return []notify.Result{{Channel: "test", OK: true}}
	}
	m.SetConfig(testNotifyConfig())
	m.Start()
	m.Stop()
	m.Stop()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); m.Enqueue(msg()) }()
	wg.Wait()
}

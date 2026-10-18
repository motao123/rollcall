package outbox

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/motao/rollcall/internal/notify"
	"github.com/motao/rollcall/internal/store"
)

// TestStartAfterStopDoesNotDispatch 防止关停后重新启动：
// Stop 已返回、无人等待新起的 worker，它们会继续 claim 并外发。
func TestStartAfterStopDoesNotDispatch(t *testing.T) {
	st := store.New(t.TempDir())
	m := testManager(st)
	m.SetConfig(testNotifyConfig())

	var sent int32
	m.send = func(*notify.Config, notify.Message) []notify.Result {
		atomic.AddInt32(&sent, 1)
		return []notify.Result{{Channel: "c1", OK: true}}
	}

	m.Stop()
	m.Enqueue(msg()) // 先入队，确保旧实现一旦启动循环就会 claim
	m.Start()
	time.Sleep(200 * time.Millisecond)

	if n := atomic.LoadInt32(&sent); n != 0 {
		t.Fatalf("Stop 之后不应再投递，实际发送 %d 次", n)
	}
}

// TestConcurrentStartStopDoesNotPanic 复现 Start/Stop 交错的 WaitGroup 误用。
// 重点不是断言某个状态，而是不允许出现 panic（计数为 0 时 Add 与 Wait 并发）。
func TestConcurrentStartStopDoesNotPanic(t *testing.T) {
	st := store.New(t.TempDir())
	m := testManager(st)

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); m.Start() }()
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); m.Stop() }()
	}
	wg.Wait()
}

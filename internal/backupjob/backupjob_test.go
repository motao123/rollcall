package backupjob

import (
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/store"
)

// TestManualRunDefersArmedTimer 回归：
// 调度循环的 timer 一旦排定就固定了 deadline，而 RunNow 会把 next 推后。
// 若到点后不重新核对 next，就会在「启动 30 秒内手动点了一次立即备份」
// 之后按原 deadline 再补跑一次，凭空多出一份重复备份（keep 上限内挤掉历史）。
func TestManualRunDefersArmedTimer(t *testing.T) {
	m, err := New(Config{
		Dir:      t.TempDir(),
		Interval: time.Minute,
		Keep:     5,
		Password: "test-password-123",
		Store:    store.New(t.TempDir()),
	})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}

	// loop 对 <1s 的等待会夹到 1s，所以把首次 deadline 设成 50ms，
	// 实际仍在启动后 1s 触发 —— 足够手动运行插在中间。
	m.mu.Lock()
	m.next = time.Now().Add(50 * time.Millisecond)
	m.mu.Unlock()

	m.Start()
	defer m.Stop()

	time.Sleep(150 * time.Millisecond)
	if _, err := m.RunNow(); err != nil {
		t.Fatalf("手动运行失败: %v", err)
	}
	// 等过原 deadline（loop 启动后 1s）+ 余量
	time.Sleep(1200 * time.Millisecond)

	files, err := m.listFiles()
	if err != nil {
		t.Fatalf("列出备份失败: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("手动运行后不应再按原 deadline 补跑，实际文件数 %d", len(files))
	}

	// next 必须被推到「本次运行 + 间隔」，而不是留在过去。
	if st := m.Status(); st.NextAt <= time.Now().Unix() {
		t.Fatalf("NextAt 未推进: %+v", st)
	}
}

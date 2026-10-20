package outbox

import (
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/store"
)

// TestNewCopiesBackoffSlice 是回归测试。
//
// Config 按值传入，但 Backoff 是切片（只有头部被复制）。调用方在 New 之后
// 修改自己的切片会直接改变投递退避时序，而这种 bug 极难定位。
func TestNewCopiesBackoffSlice(t *testing.T) {
	st := store.New(t.TempDir())
	bo := []time.Duration{time.Second}
	m := New(st, Config{Backoff: bo})

	bo[0] = time.Hour
	if got := m.cfg.Backoff[0]; got != time.Second {
		t.Fatalf("Manager 不应共享调用方的切片，实际 %v", got)
	}

	// 未提供 Backoff 时使用默认值，且同样不与默认表共享底层数组
	m2 := New(st, Config{})
	if len(m2.cfg.Backoff) != len(DefaultConfig().Backoff) {
		t.Fatalf("应回落到默认退避表，实际 %v", m2.cfg.Backoff)
	}
	m2.cfg.Backoff[0] = 42 * time.Second
	if DefaultConfig().Backoff[0] == 42*time.Second {
		t.Fatal("默认退避表被共享修改")
	}
}

package main

import (
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/store"
)

// TestSeedDemoMarksOfflineNode 防止「离线演示节点其实在线」。
//
// 旧实现先给 n08 写满到 now 的样本，再补一条 now-900；
// 环形缓冲会拒绝时间回退的点，那条样本被静默丢弃，
// 于是节点一直在线、r_offline 永不触发。
func TestSeedDemoMarksOfflineNode(t *testing.T) {
	if testing.Short() {
		t.Skip("seedDemo 会生成 40 万级样本，short 模式跳过")
	}
	st := store.New(t.TempDir())
	seedDemo(st)

	now := time.Now().Unix()

	_, last, ok := st.Latest("n08")
	if !ok {
		t.Fatal("n08 应有点位")
	}
	if age := now - last; age < 600 {
		t.Fatalf("n08 最后上报 age=%ds，应在 600 秒以上才判定离线", age)
	}

	_, lastOnline, ok := st.Latest("n01")
	if !ok {
		t.Fatal("n01 应有点位")
	}
	if age := now - lastOnline; age > 120 {
		t.Fatalf("n01 最后上报 age=%ds，应保持在线（<=120 秒）", age)
	}
}

// TestSeedDemoUptimeMonotonic 防止运行时长方向反了。
//
// 旧实现 `now-t + 13 天`：越早的样本算出的 uptime 越大
// （365 天前约 378 天、最新点只有 13 天），导出与历史曲线里
// 看起来像每次采样都重启了一次。
func TestSeedDemoUptimeMonotonic(t *testing.T) {
	if testing.Short() {
		t.Skip("seedDemo 会生成 40 万级样本，short 模式跳过")
	}
	st := store.New(t.TempDir())
	seedDemo(st)

	// 取跨度足够长的时间窗（365 天），Series 按时间升序返回。
	now := time.Now().Unix()
	pts := st.Series("n01", now-366*86400)
	if len(pts) < 100 {
		t.Fatalf("样本过少: %d", len(pts))
	}
	prev := uint64(0)
	for i, p := range pts {
		if p.Uptime < prev {
			t.Fatalf("第 %d 个点 uptime 回退: %d -> %d（时间 t=%d）", i, prev, p.Uptime, p.TS)
		}
		prev = p.Uptime
	}
	// 最新点的开机时长应覆盖整段数据（约 378 天），而不是 13 天。
	if prev < uint64(300*86400) {
		t.Fatalf("最新点 uptime=%d 秒，明显小于数据跨度（应约 378 天）", prev)
	}
}

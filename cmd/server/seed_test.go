package main

import (
	"testing"
	"time"

	"github.com/motao/rollcall/internal/store"
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

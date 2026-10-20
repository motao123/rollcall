package web

import (
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/store"
)

// TestBuildNodeViewsLiteSkipsExtras 防止 /metrics 抓取承担 sparkline 开销。
//
// 抓取路径每次都要跑：完整视图会为每个节点复制最近 20 分钟的原始序列
// 并统计告警数，节点多时是 O(n × 点数) 的额外分配，而这两个字段
// 在 Prometheus 输出里完全用不到。
func TestBuildNodeViewsLiteSkipsExtras(t *testing.T) {
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "节点一"})
	now := time.Now().Unix()
	for i := 0; i < 40; i++ {
		st.AddMetrics("n1", model.HostInfo{}, model.Metrics{TS: now - int64(i)*30, CPUPct: 42})
	}
	srv := New(st, nil, nil, Config{})

	full := srv.buildNodeViews()
	lite := srv.buildNodeViewsLite()
	if len(full) != 1 || len(lite) != 1 {
		t.Fatalf("视图数 = %d / %d", len(full), len(lite))
	}
	if len(full[0].Spark) == 0 {
		t.Fatal("完整视图应含迷你曲线")
	}
	if len(lite[0].Spark) != 0 {
		t.Fatalf("轻量视图不应构造迷你曲线，实际 %d 点", len(lite[0].Spark))
	}
	if lite[0].Latest.CPUPct != 42 || lite[0].State != full[0].State {
		t.Fatalf("轻量视图的数值字段不完整: %+v", lite[0])
	}
}

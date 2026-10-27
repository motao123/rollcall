package store

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/motao123/rollcall/internal/model"
)

// TestCPUHistPercentileAccuracy 验证分位数估算的误差在档宽一半以内。
func TestCPUHistPercentileAccuracy(t *testing.T) {
	var h cpuHist
	// 0..99 均匀分布（每档 10 个样本）
	for i := 0; i < 100; i++ {
		h.observe(float64(i))
	}
	if got := h.total(); got != 100 {
		t.Fatalf("样本数 = %d，期望 100", got)
	}
	// 容许误差 = 档宽的一半：低端 10% 档 → ±5；高端 5% 档 → ±2.5。
	cases := []struct {
		q    float64
		want float64
		tol  float64
	}{
		{0.50, 50, 6},
		{0.90, 90, 3},
		{0.95, 95, 3},
		{0.99, 99, 3},
	}
	for _, c := range cases {
		got := h.percentile(c.q)
		if math.Abs(got-c.want) > c.tol {
			t.Errorf("p%.0f = %.2f，期望 %.0f±%.0f", c.q*100, got, c.want, c.tol)
		}
	}
}

// TestCPUHistCatchesTailSpike 是本功能存在的理由：
// 94 个 20% + 6 个 90% 时，均值只有 24.2% 看起来「很闲」，
// 但 p95（第 95 个样本）落在 90 —— 这正是均值掩盖的容量风险。
//
// 注意 99:1 的极端偏斜不在本用例里：那时第 95 个样本本来就是 20，
// p95 在数学上等于 20（分位数定义），尖峰只能由 CPUMax 体现。
func TestCPUHistCatchesTailSpike(t *testing.T) {
	var h cpuHist
	for i := 0; i < 94; i++ {
		h.observe(20)
	}
	for i := 0; i < 6; i++ {
		h.observe(90)
	}

	mean := (94*20.0 + 6*90) / 100
	if mean > 25 {
		t.Fatalf("测试前提错误：均值 %.2f 应接近 24.2", mean)
	}
	if p95 := h.percentile(0.95); p95 < 85 {
		t.Errorf("p95 = %.1f，应落在 90 档（≥85），均值 %.1f 会掩盖它", p95, mean)
	}
	if p50 := h.percentile(0.50); math.Abs(p50-20) > 6 {
		t.Errorf("p50 = %.1f，应接近 20", p50)
	}
	// p99 也应在高位（第 99 个样本是 90）
	if p99 := h.percentile(0.99); p99 < 85 {
		t.Errorf("p99 = %.1f，应在 90 档", p99)
	}
}

// TestCPUHistEmpty 空直方图必须返回 0（表示"无摘要"而不是"0% 使用"）。
func TestCPUHistEmpty(t *testing.T) {
	var h cpuHist
	if !h.empty() {
		t.Error("零值直方图应视为空")
	}
	for _, q := range []float64{0.5, 0.95, 0.99} {
		if got := h.percentile(q); got != 0 {
			t.Errorf("空直方图 p%.0f = %v，期望 0", q*100, got)
		}
	}
}

// TestCPUHistClamp 越界值被夹到首/末档，NaN 被忽略（不能污染计数）。
func TestCPUHistClamp(t *testing.T) {
	var h cpuHist
	h.observe(-3)
	h.observe(120)
	h.observe(math.NaN())
	if got := h.total(); got != 2 {
		t.Fatalf("样本数 = %d，期望 2（NaN 不计）", got)
	}
	if h[0] != 1 || h[cpuHistBuckets-1] != 1 {
		t.Errorf("越界值未夹到首/末档: %v", h)
	}
}

// TestCPUHistMerge 合并等价于逐个 observe（未来若改为层间 rollup 依赖它）。
func TestCPUHistMerge(t *testing.T) {
	var a, b cpuHist
	for i := 0; i < 40; i++ {
		a.observe(30)
	}
	for i := 0; i < 60; i++ {
		b.observe(80)
	}
	want := a.percentile(0.95)
	a.merge(&b)
	if a.total() != 100 {
		t.Fatalf("合并后样本数 = %d，期望 100", a.total())
	}
	// 合并后 p95 必须落在 80 档（60% 的样本在 80）
	if got := a.percentile(0.95); got < 75 {
		t.Errorf("合并后 p95 = %.1f，期望 ≥75（原 a 的 p95 是 %.1f）", got, want)
	}
}

// TestCPUHistJSON 落盘形态：空直方图序列化成 null，非空是紧凑数组。
func TestCPUHistJSON(t *testing.T) {
	var empty cpuHist
	b, err := json.Marshal(empty)
	if err != nil || string(b) != "null" {
		t.Fatalf("空直方图应序列化为 null，得到 %s (err=%v)", b, err)
	}

	var h cpuHist
	h.observe(55)
	h.observe(95)
	b, err = json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	var back cpuHist
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("反序列化失败: %v (%s)", err, b)
	}
	if back != h {
		t.Errorf("往返不一致: %v -> %s -> %v", h, b, back)
	}

	// null 必须能反序列化成空值（老桶/1m 层）
	var fromNull cpuHist
	if err := json.Unmarshal([]byte("null"), &fromNull); err != nil {
		t.Fatalf("null 反序列化失败: %v", err)
	}
	if !fromNull.empty() {
		t.Error("null 应解成空直方图")
	}
}

// TestHistorySeriesExposesPercentiles 端到端：5m/1h 层输出 p50/p95，
// 1m 层不输出（前端据此回退到自算）。
func TestHistorySeriesExposesPercentiles(t *testing.T) {
	st := New(t.TempDir())
	node := &model.Node{ID: "n1", Name: "分位测试", Token: "t1"}
	st.UpsertNode(node)

	// 19 个样本、间隔 15 秒（base+15..base+285，全在一个 5m 桶内）：
	// 17 个 20% + 2 个 90%（10.5% 高位 → p95 落在 90 档）
	base := int64(1_700_000_000) / 300 * 300
	for i := 0; i < 19; i++ {
		cpu := 20.0
		if i >= 17 {
			cpu = 90
		}
		st.AddMetrics("n1", model.HostInfo{}, model.Metrics{
			TS: base + int64(i*15) + 15, CPUPct: cpu,
			MemUsed: 40, MemTotal: 100, DiskUsed: 30, DiskTotal: 100,
		})
	}

	pts := st.HistorySeries("n1", HistoryResolution5m, base)
	if len(pts) != 1 {
		t.Fatalf("5m 桶数 = %d，期望 1", len(pts))
	}
	if pts[0].CPUP95 < 85 {
		t.Errorf("5m 桶 p95 = %.1f，应暴露 95%% 的尖峰", pts[0].CPUP95)
	}
	// 4×20 + 1×95：p50 是第 2.5 个样本，落在 20 档（插值上限 30）
	if pts[0].CPUP50 < 15 || pts[0].CPUP50 > 30 {
		t.Errorf("5m 桶 p50 = %.1f，应落在 20–30 区间", pts[0].CPUP50)
	}
	// 均值仍然骗人 —— 这正是要补分位数的原因
	if pts[0].CPUPct > 40 {
		t.Errorf("均值 = %.1f，测试前提是它明显低于 p95", pts[0].CPUPct)
	}

	// 1m 层不挂直方图：同样数据下 p95 应为 0（前端自算）
	pts1m := st.HistorySeries("n1", HistoryResolution1m, base)
	if len(pts1m) == 0 {
		t.Fatal("1m 层应有数据")
	}
	for _, p := range pts1m {
		if p.CPUP95 != 0 {
			t.Errorf("1m 桶不应带 p95（实际 %.1f）—— 该层靠前端精确计算", p.CPUP95)
		}
	}
}

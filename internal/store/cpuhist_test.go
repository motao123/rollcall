package store

import (
	"encoding/json"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/motao123/rollcall/internal/model"
)

// TestCPUHistPercentileAccuracy 验证分位数估算的误差在档宽一半以内。
func TestCPUHistPercentileAccuracy(t *testing.T) {
	var h cpuHist
	// 0..99 均匀分布
	for i := 0; i < 100; i++ {
		h.observe(float64(i))
	}
	if got := h.total(); got != 100 {
		t.Fatalf("样本数 = %d，期望 100", got)
	}
	// 容许误差按新档宽给：中段 20–40 最宽，低端 0–1/1–2 很窄。
	const upperCap = 99.0
	cases := []struct {
		q    float64
		want float64
		tol  float64
	}{
		{0.50, 50, 11}, // 落在 40–60 档（宽 20）
		{0.90, 89, 11}, // 落在 80–90 档
		{0.95, 94, 4},  // 落在 90–95 档
		{0.99, 99, 3},  // 落在 98–100 档
	}
	for _, c := range cases {
		got := h.percentile(c.q, upperCap)
		if math.Abs(got-c.want) > c.tol {
			t.Errorf("p%.0f = %.2f，期望 %.0f±%.0f", c.q*100, got, c.want, c.tol)
		}
	}
}

// TestCPUHistIdleMachineNotOverstated 是 v1.39.1 的生产缺陷回归。
//
// 一台 CPU 长期 0.2% 的空载机器，旧档位（首档 0–10）把所有样本都塞进首档，
// 档内均匀插值给出 p50=5.0、p95=9.5 —— 而同一桶的精确峰值只有 0.7。
// 「p95 大于峰值」是用户一眼就能判断为错误的结果，必须被这条用例挡住。
func TestCPUHistIdleMachineNotOverstated(t *testing.T) {
	var h cpuHist
	for i := 0; i < 60; i++ {
		h.observe(0.2)
	}
	const exactMax = 0.7 // 同桶精确峰值（由 CPUMax 记录，与直方图无关）

	p50 := h.percentile(0.50, exactMax)
	p95 := h.percentile(0.95, exactMax)

	if p95 > exactMax+1e-9 {
		t.Errorf("p95 = %.2f 超过同桶精确峰值 %.2f —— 这就是线上看到的错误数值", p95, exactMax)
	}
	if p50 > p95+1e-9 {
		t.Errorf("p50 = %.2f 大于 p95 = %.2f，分位数必须单调", p50, p95)
	}
	// 新档位首档是 0–1，估算值不可能再跳到 9.5
	if p95 > 1.0 {
		t.Errorf("p95 = %.2f，空载机器的尾部不应超过 1%%（档位细分后）", p95)
	}
}

// TestCPUHistPercentileBoundedAndMonotonic 属性测试：
// 任意分布下都必须满足 0 ≤ p50 ≤ p90 ≤ p95 ≤ max。
// 这是分位数唯一能让人「一眼验证」的恒等式，破坏它的产品没人敢用。
func TestCPUHistPercentileBoundedAndMonotonic(t *testing.T) {
	rng := rand.New(rand.NewSource(20261028))
	for round := 0; round < 200; round++ {
		var h cpuHist
		n := 1 + rng.Intn(200)
		maxSeen := 0.0
		// 随机偏斜分布：大部分样本挤在低区间，少数尖峰冲到 80% 以上
		base := rng.Float64() * 20
		spike := 80 + rng.Float64()*20
		spikeEvery := 3 + rng.Intn(50)
		for i := 0; i < n; i++ {
			v := base + rng.Float64()*2
			if i%spikeEvery == 0 {
				v = spike
			}
			if v > maxSeen {
				maxSeen = v
			}
			h.observe(v)
		}
		p50 := h.percentile(0.50, maxSeen)
		p90 := h.percentile(0.90, maxSeen)
		p95 := h.percentile(0.95, maxSeen)
		if p50 < 0 || p50 > p90+1e-9 || p90 > p95+1e-9 {
			t.Fatalf("第 %d 轮单调性被破坏: p50=%.2f p90=%.2f p95=%.2f", round, p50, p90, p95)
		}
		if p95 > maxSeen+1e-9 {
			t.Fatalf("第 %d 轮 p95=%.2f 超过峰值 %.2f", round, p95, maxSeen)
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
	if p95 := h.percentile(0.95, 90); p95 < 85 {
		t.Errorf("p95 = %.1f，应落在 90 档（≥85），均值 %.1f 会掩盖它", p95, mean)
	}
	// 94 个样本落在 20–40 档，p50 应落在档内而不是溢出到 90
	if p50 := h.percentile(0.50, 90); math.Abs(p50-30) > 11 {
		t.Errorf("p50 = %.1f，应接近 20–40 档的中部", p50)
	}
	if p99 := h.percentile(0.99, 90); p99 < 85 {
		t.Errorf("p99 = %.1f，应在 90 档", p99)
	}
}

// TestCPUHistEmpty 空直方图必须返回 0（表示「无摘要」而不是「0% 使用」）。
func TestCPUHistEmpty(t *testing.T) {
	var h cpuHist
	if !h.empty() {
		t.Error("零值直方图应视为空")
	}
	for _, q := range []float64{0.5, 0.95, 0.99} {
		if got := h.percentile(q, 50); got != 0 {
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
	want := a.percentile(0.95, 30)
	a.merge(&b)
	if a.total() != 100 {
		t.Fatalf("合并后样本数 = %d，期望 100", a.total())
	}
	// 合并后 p95 必须落在 80 档（60% 的样本在 80）
	if got := a.percentile(0.95, 80); got < 75 {
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
	// p95 被精确峰值封顶后不可能超过 CPUMax —— 这是 v1.39.1 修掉的缺陷
	if pts[0].CPUP95 > pts[0].CPUMax+1e-9 {
		t.Errorf("p95 = %.1f 超过峰值 %.1f", pts[0].CPUP95, pts[0].CPUMax)
	}
	if pts[0].CPUP50 > pts[0].CPUP95+1e-9 {
		t.Errorf("p50 = %.1f 大于 p95 = %.1f", pts[0].CPUP50, pts[0].CPUP95)
	}
	// 17/19 个样本落在 20–40 档
	if pts[0].CPUP50 < 20 || pts[0].CPUP50 > 40 {
		t.Errorf("5m 桶 p50 = %.1f，应落在 20–40 区间", pts[0].CPUP50)
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

// writeHistorySnapshot 在临时目录写一份 history.json（未压缩，loadHistory 两种都接受）。
func writeHistorySnapshot(t *testing.T, dir string, schema int) int64 {
	t.Helper()
	base := int64(1_700_000_000) / 300 * 300
	var hist cpuHist
	for i := 0; i < 10; i++ {
		hist.observe(90)
	}
	snap := historySnapshot{
		SchemaVersion: schema,
		SavedAt:       base + 300,
		Nodes: map[string]*historyBucketSnapshot{
			"n1": {M5: []historyBucket{{
				Index: base, Count: 10, CPUAvg: 90, CPUMax: 90, CPUHist: hist,
				MemAvg: 40, DiskAvg: 30,
			}}},
		},
	}
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "history.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return base
}

// TestHistorySchema4DropsLegacyHistograms 是 schema 3 → 4 的迁移回归。
//
// 直方图的档位边界是存储语义的一部分：schema 3 的计数按旧边界统计
// （首档 0–10），在新边界下读数会得到 p95 大于峰值这种不可能的值。
// 因此加载旧文件时必须丢弃直方图，但 avg/max 与用量数据必须保留 ——
// 否则一次升级就把 30 天历史抹掉，那是更严重的倒退。
func TestHistorySchema4DropsLegacyHistograms(t *testing.T) {
	oldDir := t.TempDir()
	base := writeHistorySnapshot(t, oldDir, 3)
	stOld := New(oldDir)
	if err := stOld.LoadErr(); err != nil {
		t.Fatalf("加载 schema 3 历史失败: %v", err)
	}
	pts := stOld.HistorySeries("n1", HistoryResolution5m, base)
	if len(pts) != 1 {
		t.Fatalf("schema 3 桶数 = %d，期望 1（桶本身必须保留）", len(pts))
	}
	if pts[0].CPUP95 != 0 || pts[0].CPUP50 != 0 {
		t.Errorf("schema 3 的直方图应被丢弃（得到 p50=%.1f p95=%.1f）",
			pts[0].CPUP50, pts[0].CPUP95)
	}
	if pts[0].CPUMax != 90 || pts[0].CPUPct != 90 {
		t.Errorf("avg/max 必须保留，得到 avg=%.1f max=%.1f", pts[0].CPUPct, pts[0].CPUMax)
	}
	if pts[0].MemPct != 40 {
		t.Errorf("内存均值必须保留，得到 %.1f", pts[0].MemPct)
	}

	// 同一份数据标成 schema 4 时，直方图必须被保留（否则分位数永远拿不到）
	newDir := t.TempDir()
	base4 := writeHistorySnapshot(t, newDir, 4)
	stNew := New(newDir)
	if err := stNew.LoadErr(); err != nil {
		t.Fatalf("加载 schema 4 历史失败: %v", err)
	}
	pts4 := stNew.HistorySeries("n1", HistoryResolution5m, base4)
	if len(pts4) != 1 {
		t.Fatalf("schema 4 桶数 = %d，期望 1", len(pts4))
	}
	if pts4[0].CPUP95 == 0 {
		t.Error("schema 4 的直方图应被保留，但 p95 为 0")
	}
}

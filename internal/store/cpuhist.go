package store

import (
	"encoding/json"
	"fmt"
	"math"
)

// cpuHistBuckets 是 CPU 使用率的直方图档数。
//
// 为什么用直方图而不是 t-digest（参考实现见 Argus/internal/tdigest）：
//
//	本项目的历史桶有硬内存约束 —— 每桶 64 字节、40 节点满保留期约 110 万个桶、
//	常驻约 70MB（见 historyBucket 注释）。t-digest 单桶约 100–200 字节，
//	挂满两层会让常驻内存翻倍以上；而这里要回答的问题只有一个是非判断
//	（"这个桶里 CPU 是否接近饱和"）。直方图 24 字节/桶，可 O(档数) 求任意
//	分位，且天然可合并（未来若改成层间 rollup 也不需要换算法）。
//
// ★ 档位在 v1.39.1 重新设计过（档数不变，内存一分没多）：
// 旧版首档是 0–10，对空载机器太宽 —— 一台 0.2% 的机器全部样本落在首档，
// 档内均匀插值给出 p50=5.0、p95=9.5，而它真实峰值只有 0.7，
// 于是出现「p95 大于峰值」这种用户一眼就能看出不可能的数值。
// 新档位两端加密、中段放宽：0–1/1–2 区分空载、2–5/5–10 轻载、
// 10–20/20–40/40–60 正常区间、60–80 接近容量、
// 80–90/90–95/95–98/98–100 饱和区分（这才是本功能要回答的问题）。
const cpuHistBuckets = 12

// cpuHistUpper 是各档的上界（最后一档含 100）。必须严格递增。
var cpuHistUpper = [cpuHistBuckets]float64{1, 2, 5, 10, 20, 40, 60, 80, 90, 95, 98, 100}

// cpuHist 是 CPU 使用率的分档计数（uint16：1 小时桶在 5 秒上报下 720 个样本也不会溢出）。
type cpuHist [cpuHistBuckets]uint16

// observe 记录一个样本。越界值被夹到首/末档（负值来自浮点误差，>100 来自采集口径）。
func (h *cpuHist) observe(pct float64) {
	if math.IsNaN(pct) {
		return
	}
	idx := 0
	for idx < cpuHistBuckets-1 && pct >= cpuHistUpper[idx] {
		idx++
	}
	h[idx]++
}

// total 返回样本总数。
func (h *cpuHist) total() int {
	n := 0
	for _, c := range h {
		n += int(c)
	}
	return n
}

// empty 判断是否没有任何样本（老桶 / 未挂直方图的层）。
func (h *cpuHist) empty() bool { return h.total() == 0 }

// percentile 用线性插值估算分位数（q ∈ (0,1]）。空直方图返回 0。
//
// upperCap 传该桶的**精确峰值**（CPUMax）：档内插值假设"档内均匀分布"，
// 这个假设在样本高度集中时会把结果推到档的右边缘 —— 例如整桶样本都是 0.2%
// 时，落在 0–1 档却仍可能插值出 0.9。若不加约束，就会出现 p95 大于峰值的
// 数值（v1.39.0 在生产的真实表现是 p50=5.0、p95=9.5、max=0.7）。
// 用精确峰值封顶后，p50 ≤ p95 ≤ max 这条用户一眼能验证的恒等式必然成立。
// upperCap ≤ 0（无样本或全零）时不做封顶，直接返回估算值。
//
// 精度：误差上界为档宽的一半（空载段 ≤0.5、饱和段 ≤1~2.5、中段 ≤10）。
// 必须与 CPUMax 配合读：
//   - p50 看稳态水位、p95 看"较差的 5% 样本"、max 看最坏瞬时值。
//   - 极端偏斜分布（例如 99 个 20% + 1 个 95%）下 p95 在数学上就等于 20
//     （第 95 个样本本来就在 20 那一档），尖峰只能由 max 或 p99 体现 ——
//     这是分位数的定义决定的，不是直方图的误差。
func (h *cpuHist) percentile(q float64, upperCap float64) float64 {
	n := h.total()
	if n == 0 {
		return 0
	}
	if q <= 0 {
		q = 0.0001
	}
	if q > 1 {
		q = 1
	}
	target := q * float64(n)
	var cum float64
	for i, c := range h {
		if c == 0 {
			continue
		}
		next := cum + float64(c)
		if next >= target {
			lower := 0.0
			if i > 0 {
				lower = cpuHistUpper[i-1]
			}
			upper := cpuHistUpper[i]
			// 在档内按 (target-cum)/c 的比例插值
			pos := (target - cum) / float64(c)
			if pos < 0 {
				pos = 0
			}
			if pos > 1 {
				pos = 1
			}
			return clampPercentile(lower+(upper-lower)*pos, upperCap)
		}
		cum = next
	}
	// 理论上不会走到这里（target ≤ n 且累计到最后一档必达），
	// 兜底仍要封顶，避免出现 100 > max 的异常值。
	return clampPercentile(100, upperCap)
}

// percentileClamped 是 percentile 的内层实现：先估算再按 upperCap 封顶。
// 单独拆出来是为了让上面的循环保持「只有一个出口」的可读性。
func clampPercentile(est, upperCap float64) float64 {
	if upperCap > 0 && est > upperCap {
		return upperCap
	}
	if est < 0 {
		return 0
	}
	return est
}

// merge 把另一份直方图并入（合并同层相邻桶时使用；当前三层各自累加，
// 保留该能力以便未来改为层间 rollup）。
func (h *cpuHist) merge(o *cpuHist) {
	for i := range h {
		sum := int(h[i]) + int(o[i])
		if sum > math.MaxUint16 {
			sum = math.MaxUint16
		}
		h[i] = uint16(sum)
	}
}

// MarshalJSON / UnmarshalJSON：以紧凑数组下发/落盘，空直方图直接省略
// （由外层 omitempty 控制，这里只需保证 0 值可序列化）。
func (h cpuHist) MarshalJSON() ([]byte, error) {
	if h.total() == 0 {
		return []byte("null"), nil
	}
	return json.Marshal([cpuHistBuckets]uint16(h))
}

func (h *cpuHist) UnmarshalJSON(b []byte) error {
	if string(b) == "null" || len(b) == 0 {
		*h = cpuHist{}
		return nil
	}
	var arr [cpuHistBuckets]uint16
	if err := json.Unmarshal(b, &arr); err != nil {
		return fmt.Errorf("cpuHist: %w", err)
	}
	*h = arr
	return nil
}

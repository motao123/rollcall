package store

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/motao/rollcall/internal/model"
)

// buildStore 构造贴近生产规模的 Store：nodes 个节点，每个填满 24h 环形缓冲（2880 点）。
// 缓存复用，避免多个子测试重复构建（100 节点 × 2880 点 = 28.8 万次写入）。
var (
	storeCache   = map[int]*Store{}
	storeCacheMu sync.Mutex
)

func buildStore(t testing.TB, nodes int) *Store {
	storeCacheMu.Lock()
	defer storeCacheMu.Unlock()
	if s, ok := storeCache[nodes]; ok {
		return s
	}
	s := newBenchStore(t, nodes)
	storeCache[nodes] = s
	return s
}

// newBenchStore 创建不进入缓存的生产规模 Store。
// 不同 Benchmark 函数之间不能共享缓存：t.TempDir() 会在前一个
// Benchmark 结束后删除目录，后一个函数复用缓存会写入已消失的路径。
func newBenchStore(t testing.TB, nodes int) *Store {
	s := New(t.TempDir())
	now := time.Now().Unix()
	for i := 0; i < nodes; i++ {
		id := NewID("n")
		s.UpsertNode(&model.Node{ID: id, Name: "节点", Token: NewToken()})
		s.AddMetrics(id, model.HostInfo{Hostname: "h", OS: "Debian 12"}, model.Metrics{})
		for ts := now - 86400; ts <= now; ts += 30 {
			s.AddMetrics(id, model.HostInfo{}, model.Metrics{
				TS: ts, CPUPct: 42.5, Load1: 1.2, Load5: 1.1, Load15: 1.0,
				MemUsed: 2 << 30, MemTotal: 8 << 30, SwapUsed: 0, SwapTotal: 2 << 30,
				DiskUsed: 20 << 30, DiskTotal: 60 << 30,
				NetRx: 1 << 40, NetTx: 3 << 39,
				TCPConn: 120, Uptime: 86400 * 13, Procs: 410,
			})
		}
	}
	s.UpsertService(&model.Service{ID: NewID("s"), Name: "svc", Type: "tcp", Target: "1.2.3.4:80", Interval: 60, Enabled: true})
	s.UpsertRule(&model.AlertRule{ID: NewID("r"), Name: "规则", Metric: "cpu", Op: "gt", Threshold: 90, Enabled: true})
	return s
}

// TestSaveDurationScalesSane 量化 Save() 耗时随节点规模的增长，并守住上界。
//
// 背景：修复 F1（concurrent map iteration and map write）时，把快照深拷贝
// 从「锁外 marshal」改成了「锁内深拷贝」。这是正确性必需，代价是 RLock
// 窗口变长 —— 期间 Agent 上报（AddMetrics 要拿写锁）会被阻塞。
//
// 为什么不直接测「锁持有时长」：
// 任何靠时序交错去卡锁窗口的测法都不可靠 —— 写入方可能在 Save 拿到
// RLock 之前就抢到锁（测出恒为 0），也可能被 GC 停顿与调度延迟污染
// （实测曾得到「阻塞恒定 ~10ms 不随数据量增长」的假象，
// 与 Save 总耗时 3→185ms 明显矛盾）。
//
// 因此这里测**总耗时**，并用它作为锁持有时间的上界：
// Save = [锁内深拷贝] + [锁外 json.Marshal] + [锁外写盘]，
// 所以 lockHold ≤ total。总耗时可控即证明锁窗口可控。
func TestSaveDurationScalesSane(t *testing.T) {
	// -race 的插桩开销会让耗时上界失去意义，交给基准测试衡量真实性能。
	if raceEnabled {
		t.Skip("race 构建不校验绝对耗时")
	}
	// 上界按最坏情况给足余量：Agent 上报间隔 30s、HTTP 超时 15s，
	// 只要单次 Save 远低于秒级，锁窗口就不可能造成上报失败。
	const bound = 2 * time.Second

	for _, nodes := range []int{1, 40, 100} {
		s := buildStore(t, nodes)

		// 3 次已足以暴露数量级退化；6 次会让 go test ./... 多花几十秒。
		const trials = 3
		var sum, max time.Duration
		for i := 0; i < trials; i++ {
			t0 := time.Now()
			if err := s.Save(); err != nil {
				t.Fatalf("Save 失败: %v", err)
			}
			d := time.Since(t0)
			sum += d
			if d > max {
				max = d
			}
		}
		avg := sum / trials

		t.Logf("节点数 %3d │ 单次 Save 平均 %7.2fms │ 最大 %7.2fms │ (锁持有 ≤ 此值)",
			nodes, float64(avg.Microseconds())/1000, float64(max.Microseconds())/1000)

		if max > bound {
			t.Errorf("节点数 %d：单次 Save 最大 %v 超过上界 %v，锁窗口可能影响上报",
				nodes, max, bound)
		}
	}
}

// BenchmarkSaveTotal 测量 Save() 端到端耗时（深拷贝 + JSON 序列化 + 写盘）。
func BenchmarkSaveTotal(b *testing.B) {
	for _, nodes := range []int{1, 40, 100} {
		s := buildStore(b, nodes)
		b.Run(strconv.Itoa(nodes)+"nodes", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if err := s.Save(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

package store

// 机群平均序列：为节点详情页的"机群均值参考线"提供数据。
//
// 口径（与概览 KPI 一致）：机群均值 = 有数据节点的**节点平均值的平均**
//（不是全体样本的全局平均）——这样一台多核大内存机器不会压过单核小机，
// 与管理员在概览看到的数字能对上。
//
// 两条来源路径：
//   - 聚合层（7d/30d/365d → 1m/5m/1h）：桶按墙钟对齐，同 Index 跨节点平均
//   - 原始环（1h/6h/24h）：原始点跨节点不对齐，先按 1 分钟桶归约出
//     每节点每分钟的平均，再跨节点平均——参考线比主线粗是预期行为

import (
	"sort"

	"github.com/motao123/rollcall/internal/model"
)

// FleetPoint 是机群平均序列的一个点（百分比口径）。
type FleetPoint struct {
	TS   int64   `json:"ts"`
	CPU  float64 `json:"cpu"`
	Mem  float64 `json:"mem"`
	Disk float64 `json:"disk"`
}

type fleetAcc struct {
	cpu, mem, disk float64
	n              int
}

func (a *fleetAcc) add(cpu, mem, disk float64) {
	a.cpu += cpu
	a.mem += mem
	a.disk += disk
	a.n++
}

// FleetSeries 返回机群平均序列。
// resolution 为聚合层名（1m/5m/1h）时读聚合层；
// 为 HistoryResolutionRaw 时从原始环按 1 分钟桶归约。
func (s *Store) FleetSeries(resolution string, since int64) []model.FleetPoint {
	s.mu.RLock()
	defer s.mu.RUnlock()

	accs := map[int64]*fleetAcc{}
	if resolution == HistoryResolutionRaw {
		const step = int64(60)
		for _, r := range s.series {
			if r == nil || r.n == 0 {
				continue
			}
			// 每节点先按分钟归约（节点内平均），再跨节点平均
			perNode := map[int64]*fleetAcc{}
			for _, p := range r.Slice() {
				if p.TS < since {
					continue
				}
				idx := p.TS - p.TS%step
				a := perNode[idx]
				if a == nil {
					a = &fleetAcc{}
					perNode[idx] = a
				}
				a.add(p.CPUPct, nodePct(p.MemUsed, p.MemTotal), nodePct(p.DiskUsed, p.DiskTotal))
			}
			for idx, a := range perNode {
				if a.n == 0 {
					continue
				}
				f := accs[idx]
				if f == nil {
					f = &fleetAcc{}
					accs[idx] = f
				}
				f.add(a.cpu/float64(a.n), a.mem/float64(a.n), a.disk/float64(a.n))
			}
		}
	} else {
		for _, h := range s.history {
			if h == nil {
				continue
			}
			for _, b := range h.ring(resolution).since(since) {
				if b.Count == 0 {
					continue
				}
				f := accs[b.Index]
				if f == nil {
					f = &fleetAcc{}
					accs[b.Index] = f
				}
				f.add(float64(b.CPUAvg), float64(b.MemAvg), float64(b.DiskAvg))
			}
		}
	}

	out := make([]model.FleetPoint, 0, len(accs))
	for idx, a := range accs {
		if a.n == 0 {
			continue
		}
		out = append(out, model.FleetPoint{
			TS:   idx,
			CPU:  round1F(a.cpu / float64(a.n)),
			Mem:  round1F(a.mem / float64(a.n)),
			Disk: round1F(a.disk / float64(a.n)),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS < out[j].TS })
	return out
}

func nodePct(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) / float64(total) * 100
}

func round1F(f float64) float64 {
	return float64(int(f*10+0.5)) / 10
}

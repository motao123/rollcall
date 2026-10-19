package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"time"

	"github.com/motao/rollcall/internal/model"
)

// 历史分层：24h 原始点仍在 state.json；更长时间跨度由三级聚合支撑，
// 独立写入 history.json，避免主快照随保留期线性膨胀。
const (
	HistoryResolutionRaw = "raw"
	HistoryResolution1m  = "1m"
	HistoryResolution5m  = "5m"
	HistoryResolution1h  = "1h"

	// schema 2 起聚合桶记录 memUsed/diskUsed 总和，不再用最新总量反推平均值。
	historySchemaVersion = 2

	// HistorySaveInterval 是历史快照的落盘间隔。
	// 崩溃最多丢失该窗口的聚合点，原始 24h 会用于重启后回填。
	HistorySaveInterval = 5 * time.Minute

	// maxTierBuckets 限制任一聚合层的桶数，避免超大保留期耗尽内存。
	maxTierBuckets = 1_000_000
)

type historyTier struct {
	name     string
	interval int64 // 秒
	keep     int   // 桶数量
}

var (
	tier1m = historyTier{name: HistoryResolution1m, interval: 60, keep: 7 * 24 * 60}
	tier5m = historyTier{name: HistoryResolution5m, interval: 300, keep: 30 * 24 * 12}
	tier1h = historyTier{name: HistoryResolution1h, interval: 3600, keep: 365 * 24}
)

// HistoryRetention 是三级聚合各自的保留时长。
// 必须在创建任何 Store 之前配置；单进程内只应在启动阶段调用一次。
type HistoryRetention struct {
	Tier1m time.Duration
	Tier5m time.Duration
	Tier1h time.Duration
}

// DefaultHistoryRetention 与 v1.5.0 起的固定值保持一致。
func DefaultHistoryRetention() HistoryRetention {
	return HistoryRetention{
		Tier1m: 7 * 24 * time.Hour,
		Tier5m: 30 * 24 * time.Hour,
		Tier1h: 365 * 24 * time.Hour,
	}
}

// ConfigureHistory 校验并应用聚合保留期。
func ConfigureHistory(r HistoryRetention) error {
	build := func(name string, interval int64, d time.Duration) (historyTier, error) {
		if d < time.Duration(interval)*time.Second {
			return historyTier{}, fmt.Errorf("%s 保留期不能短于一个聚合窗口（%ds）", name, interval)
		}
		if d > 10*365*24*time.Hour {
			return historyTier{}, fmt.Errorf("%s 保留期不能超过 10 年", name)
		}
		if d%(time.Duration(interval)*time.Second) != 0 {
			return historyTier{}, fmt.Errorf("%s 保留期必须是 %ds 的整数倍", name, interval)
		}
		keep := int(d / (time.Duration(interval) * time.Second))
		if keep > maxTierBuckets {
			return historyTier{}, fmt.Errorf("%s 保留期对应 %d 个桶，超过上限 %d", name, keep, maxTierBuckets)
		}
		return historyTier{name: name, interval: interval, keep: keep}, nil
	}
	minTier, err := build(HistoryResolution1m, 60, r.Tier1m)
	if err != nil {
		return err
	}
	fiveTier, err := build(HistoryResolution5m, 300, r.Tier5m)
	if err != nil {
		return err
	}
	hourTier, err := build(HistoryResolution1h, 3600, r.Tier1h)
	if err != nil {
		return err
	}
	// 更细的聚合层不应比更粗的层保留更久，否则时间范围会自相矛盾。
	if r.Tier1m > r.Tier5m || r.Tier5m > r.Tier1h {
		return errors.New("聚合保留期必须满足 1m ≤ 5m ≤ 1h")
	}
	tier1m, tier5m, tier1h = minTier, fiveTier, hourTier
	return nil
}

func tierByName(name string) *historyTier {
	switch name {
	case HistoryResolution1m:
		return &tier1m
	case HistoryResolution5m:
		return &tier5m
	case HistoryResolution1h:
		return &tier1h
	}
	return nil
}

// historyBucket 是一个聚合桶。字段刻意压缩：
// 40 节点满保留期约 110 万个桶，每桶 64 字节，常驻内存约 70MB。
type historyBucket struct {
	Index     int64   `json:"b"`  // 桶起始 unix 秒
	Count     uint32  `json:"c"`  // 样本数
	CPUAvg    float32 `json:"ca"` // CPU 平均
	CPUMax    float32 `json:"cm"` // CPU 峰值
	MemAvg    float32 `json:"ma"` // 内存使用率平均
	DiskAvg   float32 `json:"da"` // 磁盘使用率平均
	Load1Avg  float32 `json:"l"`  // 1 分钟负载平均
	MemTotal  uint64  `json:"mt"` // 桶内最新内存总量
	DiskTotal uint64  `json:"dt"` // 桶内最新磁盘总量
	NetRx     uint64  `json:"rx"` // 桶内接收增量
	NetTx     uint64  `json:"tx"` // 桶内发送增量
	// schema 2：绝对用量总和，避免用「最新总量 × 平均百分比」反推，
	// 内存/磁盘扩容后会系统性失真。旧桶该字段为 0 且 UsageOK=false。
	MemUsedSum  uint64 `json:"mus,omitempty"`
	DiskUsedSum uint64 `json:"dus,omitempty"`
	UsageOK     bool   `json:"uo,omitempty"`
}

// bucketRing 是按时间升序排列的定长聚合桶缓冲。
// 首次写入时才分配，避免为从未上报的节点预留内存。
type bucketRing struct {
	interval int64
	size     int
	buf      []historyBucket
	head     int
	n        int
}

func newBucketRing(t historyTier) *bucketRing {
	return &bucketRing{interval: t.interval, size: t.keep}
}

func (r *bucketRing) add(m model.Metrics, prev *model.Metrics, hasPrev bool) {
	if r.buf == nil {
		r.buf = make([]historyBucket, r.size)
	}
	idx := m.TS / r.interval * r.interval
	if idx < 0 {
		return
	}
	var b *historyBucket
	if r.n > 0 {
		last := &r.buf[(r.head-1+r.size)%r.size]
		if idx < last.Index {
			return
		}
		if idx == last.Index {
			b = last
		}
	}
	if b == nil {
		nb := historyBucket{Index: idx, UsageOK: true}
		r.buf[r.head] = nb
		r.head = (r.head + 1) % r.size
		if r.n < r.size {
			r.n++
		} else {
			// 覆盖最旧桶后，逻辑起点随 head 前移。
		}
		b = &r.buf[(r.head-1+r.size)%r.size]
	}
	if !b.UsageOK {
		// 旧 schema 桶没有绝对用量总和；单侧损坏（只有内存或只有磁盘
		// 的 total 缺失）也会走到这里。只回填确实缺失的一侧，
		// 不能把另一侧仍然有效的总和用平均值覆盖掉。
		if b.Count > 0 {
			// Count 与总量都直接来自 JSON，普通乘法可能 uint64 溢出后
			// 回绕成一个极小的值，让历史聚合看起来「用量骤降」。
			if b.MemUsedSum == 0 || b.MemTotal == 0 {
				b.MemUsedSum = satMul(pctBytes(b.MemTotal, float64(b.MemAvg)), uint64(b.Count))
			}
			if b.DiskUsedSum == 0 || b.DiskTotal == 0 {
				b.DiskUsedSum = satMul(pctBytes(b.DiskTotal, float64(b.DiskAvg)), uint64(b.Count))
			}
		}
		b.UsageOK = true
	}
	b.Count++
	cpu := float32(m.CPUPct)
	if b.Count == 1 {
		b.CPUAvg = cpu
	} else {
		b.CPUAvg += (cpu - b.CPUAvg) / float32(b.Count)
	}
	if cpu > b.CPUMax {
		b.CPUMax = cpu
	}
	memPct := pct(m.MemUsed, m.MemTotal)
	if b.Count == 1 {
		b.MemAvg = float32(memPct)
	} else {
		b.MemAvg += (float32(memPct) - b.MemAvg) / float32(b.Count)
	}
	diskPct := pct(m.DiskUsed, m.DiskTotal)
	if b.Count == 1 {
		b.DiskAvg = float32(diskPct)
	} else {
		b.DiskAvg += (float32(diskPct) - b.DiskAvg) / float32(b.Count)
	}
	load := float32(m.Load1)
	if b.Count == 1 {
		b.Load1Avg = load
	} else {
		b.Load1Avg += (load - b.Load1Avg) / float32(b.Count)
	}
	b.MemTotal = m.MemTotal
	b.DiskTotal = m.DiskTotal
	b.MemUsedSum = addSat(b.MemUsedSum, m.MemUsed)
	b.DiskUsedSum = addSat(b.DiskUsedSum, m.DiskUsed)
	if hasPrev {
		b.NetRx += counterDelta(prev.NetRx, m.NetRx)
		b.NetTx += counterDelta(prev.NetTx, m.NetTx)
	}
}

func addSat(a, b uint64) uint64 {
	if ^uint64(0)-a < b {
		return ^uint64(0)
	}
	return a + b
}

// appendLoaded 用于从磁盘恢复：排序后仅保留最近 size 个桶。
func (r *bucketRing) appendLoaded(b historyBucket) {
	if b.Index <= 0 {
		return
	}
	if r.buf == nil {
		r.buf = make([]historyBucket, r.size)
	}
	if r.n > 0 {
		last := &r.buf[(r.head-1+r.size)%r.size]
		if b.Index < last.Index {
			return
		}
		if b.Index == last.Index {
			*last = b
			return
		}
	}
	r.buf[r.head] = b
	r.head = (r.head + 1) % r.size
	if r.n < r.size {
		r.n++
	}
}

func (r *bucketRing) slice() []historyBucket {
	if r == nil || r.n == 0 {
		return nil
	}
	out := make([]historyBucket, 0, r.n)
	start := (r.head - r.n + r.size) % r.size
	for i := 0; i < r.n; i++ {
		out = append(out, r.buf[(start+i)%r.size])
	}
	return out
}

func (r *bucketRing) since(ts int64) []historyBucket {
	all := r.slice()
	for i, b := range all {
		if b.Index >= ts {
			return all[i:]
		}
	}
	return nil
}

func counterDelta(prev, cur uint64) uint64 {
	// 计数器回绕/节点重启会让 cur < prev，此时按从 0 重新累计处理。
	if cur >= prev {
		return cur - prev
	}
	return cur
}

func pct(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) / float64(total) * 100
}

type nodeHistory struct {
	lastTS int64 // 已聚合的最新原始样本时间，用于重启后只回放增量
	m1     *bucketRing
	m5     *bucketRing
	h1     *bucketRing
}

func newNodeHistory() *nodeHistory {
	return &nodeHistory{
		m1: newBucketRing(tier1m),
		m5: newBucketRing(tier5m),
		h1: newBucketRing(tier1h),
	}
}

func (h *nodeHistory) ring(name string) *bucketRing {
	switch name {
	case HistoryResolution1m:
		return h.m1
	case HistoryResolution5m:
		return h.m5
	case HistoryResolution1h:
		return h.h1
	}
	return nil
}

func (h *nodeHistory) hasData() bool {
	return h.m1.n > 0 || h.m5.n > 0 || h.h1.n > 0
}

func (h *nodeHistory) add(m model.Metrics, prev *model.Metrics, hasPrev bool) {
	h.m1.add(m, prev, hasPrev)
	h.m5.add(m, prev, hasPrev)
	h.h1.add(m, prev, hasPrev)
	if m.TS > h.lastTS {
		h.lastTS = m.TS
	}
}

// addHistoryLocked 在持有 s.mu 写锁时调用。
func (s *Store) addHistoryLocked(nodeID string, m model.Metrics, prev *model.Metrics, hasPrev bool) {
	h := s.history[nodeID]
	if h == nil {
		h = newNodeHistory()
		s.history[nodeID] = h
	}
	h.add(m, prev, hasPrev)
	s.historyDirty = true
}

// ---------- 持久化 ----------

type historyBucketSnapshot struct {
	LastTS int64           `json:"lastTs"`
	M1     []historyBucket `json:"m1,omitempty"`
	M5     []historyBucket `json:"m5,omitempty"`
	H1     []historyBucket `json:"h1,omitempty"`
}

type historySnapshot struct {
	SchemaVersion int                               `json:"schemaVersion"`
	SavedAt       int64                             `json:"savedAt"`
	Nodes         map[string]*historyBucketSnapshot `json:"nodes"`
}

// loadHistory 在 main 快照载入后调用：
//   - history.json 缺失或损坏时，用 24h 原始点重建三级聚合
//   - 正常恢复时，只回放 lastTs 之后的原始点，避免重复累计
//   - 更高 schema 拒绝启动，避免旧程序静默覆盖新结构
func (s *Store) loadHistory() error {
	b, err := os.ReadFile(s.historyPath)
	if errors.Is(err, os.ErrNotExist) {
		s.mu.Lock()
		s.rebuildHistoryFromRawLocked()
		s.mu.Unlock()
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取历史快照 %s: %w", s.historyPath, err)
	}
	raw, decodeErr := decodeSnapshot(b)
	var snap historySnapshot
	if decodeErr == nil {
		decodeErr = json.Unmarshal(raw, &snap)
	}
	if decodeErr != nil {
		bak := s.historyPath + ".corrupt-" + time.Now().Format("20060102-150405")
		if renameErr := os.Rename(s.historyPath, bak); renameErr != nil {
			// 隔离失败才是当前故障，包装 renameErr；decodeErr 另有日志。
			return fmt.Errorf("历史快照损坏且无法隔离 %s: %w", s.historyPath, renameErr)
		}
		log.Printf("历史快照损坏，已备份到 %s，将从 24h 原始数据重建: %v", bak, decodeErr)
		s.mu.Lock()
		s.rebuildHistoryFromRawLocked()
		s.mu.Unlock()
		return nil
	}
	if snap.SchemaVersion > historySchemaVersion {
		// 不要重命名：改名只能挡住一次启动。
		// 首次启动改名后返回错误，第二次启动就「找不到文件」→ 用 24h 原始点
		// 重建并覆盖，把新版本写入的历史永久丢掉。保持文件原样，
		// 每次启动都拒绝，直到运维升级服务端。
		return fmt.Errorf("历史快照 schema 版本 %d 高于当前支持的 %d（%s）；请升级服务端后再启动",
			snap.SchemaVersion, historySchemaVersion, s.historyPath)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for id, nh := range snap.Nodes {
		if nh == nil {
			continue
		}
		h := newNodeHistory()
		h.lastTS = nh.LastTS
		s.loadBuckets(h.m1, nh.M1)
		s.loadBuckets(h.m5, nh.M5)
		s.loadBuckets(h.h1, nh.H1)
		s.history[id] = h
	}
	// 补齐上次落盘之后尚未聚合的原始点。
	for id, r := range s.series {
		pts := r.Slice()
		if len(pts) == 0 {
			continue
		}
		h := s.history[id]
		if h == nil {
			h = newNodeHistory()
			s.history[id] = h
			s.replayHistoryLocked(h, pts, 0)
			continue
		}
		if h.lastTS == 0 && h.hasData() {
			// 异常旧文件没有 lastTs：保留已有聚合，不猜回放范围。
			continue
		}
		s.replayHistoryLocked(h, pts, h.lastTS)
	}
	s.historyDirty = true // 合并结果需要落盘
	return nil
}

func (s *Store) loadBuckets(r *bucketRing, buckets []historyBucket) {
	if len(buckets) == 0 {
		return
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].Index < buckets[j].Index })
	for _, b := range buckets {
		r.appendLoaded(sanitizeBucket(b))
	}
}

// maxBucketCount 是单个聚合桶允许的样本数上限。
// 最细粒度是 1 分钟桶：即使每 30 秒上报一次也只有 2 个样本，
// 留到百万级已经远超合理范围。
const maxBucketCount = 1_000_000

// sanitizeBucket 修正从磁盘读入的聚合桶。
//
// history.json 可能被手工编辑、被截断写入，或来自字段语义不同的旧版本：
//   - Count 是增量平均的分母，为 0 时上一次计算的结果会被整体丢弃；
//   - Count 也用于回填绝对用量总和，超大值经饱和乘法会顶到 uint64 上限；
//   - 百分比为 NaN/Inf 时 JSON 无法编码，会让整份历史快照永远写不出去。
func sanitizeBucket(b historyBucket) historyBucket {
	if b.Count > maxBucketCount {
		b.Count = maxBucketCount
	}
	b.CPUAvg = clampPct(b.CPUAvg)
	b.CPUMax = clampPct(b.CPUMax)
	b.MemAvg = clampPct(b.MemAvg)
	b.DiskAvg = clampPct(b.DiskAvg)
	if v := float64(b.Load1Avg); math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		b.Load1Avg = 0
	}
	if b.MemUsedSum > 0 && b.MemTotal == 0 {
		// 没有总量却有用量总和：无法再计算百分比，标记为不可用重新累计。
		b.UsageOK = false
	}
	if b.DiskUsedSum > 0 && b.DiskTotal == 0 {
		// 磁盘侧同样要检查：否则 HistorySeries 会返回
		// DiskUsed > 0 而 DiskTotal == 0 的自相矛盾点。
		b.UsageOK = false
	}
	return b
}

func clampPct(v float32) float32 {
	f := float64(v)
	if math.IsNaN(f) || f < 0 {
		return 0
	}
	if f > 100 {
		return 100
	}
	return v
}

func (s *Store) rebuildHistoryFromRawLocked() {
	for id, r := range s.series {
		pts := r.Slice()
		if len(pts) == 0 {
			continue
		}
		h := newNodeHistory()
		s.replayHistoryLocked(h, pts, 0)
		s.history[id] = h
	}
	s.historyDirty = true
}

// replayHistoryLocked 按时间顺序把原始点回放到三级聚合。
// fromTS 之前的点只用于提供网络计数器的前值。
func (s *Store) replayHistoryLocked(h *nodeHistory, pts []model.Metrics, fromTS int64) {
	var prev *model.Metrics
	for i := range pts {
		p := pts[i]
		if p.TS <= fromTS {
			prev = &pts[i]
			continue
		}
		h.add(p, prev, prev != nil)
		prev = &pts[i]
	}
}

// SaveHistory 把三级聚合写入 history.json（gzip + 原子替换）。
func (s *Store) SaveHistory() error {
	s.historySaveMu.Lock()
	defer s.historySaveMu.Unlock()

	s.mu.Lock()
	if !s.historyDirty {
		s.mu.Unlock()
		return nil
	}
	s.historyDirty = false
	snap := historySnapshot{
		SchemaVersion: historySchemaVersion,
		SavedAt:       time.Now().Unix(),
		Nodes:         make(map[string]*historyBucketSnapshot, len(s.history)),
	}
	for id, h := range s.history {
		snap.Nodes[id] = &historyBucketSnapshot{
			LastTS: h.lastTS,
			M1:     h.m1.slice(),
			M5:     h.m5.slice(),
			H1:     h.h1.slice(),
		}
	}
	s.mu.Unlock()

	raw, err := json.Marshal(&snap)
	if err != nil {
		s.markHistoryDirty()
		return fmt.Errorf("序列化历史快照: %w", err)
	}
	enc, err := encodeSnapshot(raw)
	if err != nil {
		s.markHistoryDirty()
		return fmt.Errorf("压缩历史快照: %w", err)
	}
	tmp := s.historyPath + ".tmp"
	if err := os.WriteFile(tmp, enc, 0o600); err != nil {
		s.markHistoryDirty()
		return fmt.Errorf("写入历史快照: %w", err)
	}
	if err := os.Rename(tmp, s.historyPath); err != nil {
		s.markHistoryDirty()
		return fmt.Errorf("替换历史快照: %w", err)
	}
	return nil
}

func (s *Store) markHistoryDirty() {
	s.mu.Lock()
	s.historyDirty = true
	s.mu.Unlock()
}

// StartHistoryAutoSave 启动历史快照的定期落盘。
func (s *Store) StartHistoryAutoSave(every time.Duration) {
	if every <= 0 {
		every = HistorySaveInterval
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for range t.C {
			if err := s.SaveHistory(); err != nil {
				log.Printf("历史快照保存失败: %v", err)
			}
		}
	}()
}

// ---------- 查询 ----------

// HistorySeries 返回降采样后的历史点。
// resolution 仅接受 1m/5m/1h；since 为 unix 秒下界。
func (s *Store) HistorySeries(nodeID, resolution string, since int64) []model.HistoryPoint {
	t := tierByName(resolution)
	if t == nil {
		return nil
	}
	s.mu.RLock()
	h := s.history[nodeID]
	var buckets []historyBucket
	if h != nil {
		buckets = h.ring(resolution).since(since)
	}
	s.mu.RUnlock()

	out := make([]model.HistoryPoint, 0, len(buckets))
	for _, b := range buckets {
		p := model.HistoryPoint{
			TS:         b.Index,
			CPUPct:     float64(b.CPUAvg),
			CPUMax:     float64(b.CPUMax),
			MemPct:     float64(b.MemAvg),
			MemTotal:   b.MemTotal,
			DiskPct:    float64(b.DiskAvg),
			DiskTotal:  b.DiskTotal,
			Load1:      float64(b.Load1Avg),
			NetRxDelta: b.NetRx,
			NetTxDelta: b.NetTx,
			Count:      b.Count,
		}
		if b.UsageOK && b.Count > 0 {
			p.MemUsed = b.MemUsedSum / uint64(b.Count)
			p.DiskUsed = b.DiskUsedSum / uint64(b.Count)
		} else {
			// 兼容 schema 1 桶：只能按最新总量回算。
			p.MemUsed = pctBytes(b.MemTotal, p.MemPct)
			p.DiskUsed = pctBytes(b.DiskTotal, p.DiskPct)
		}
		out = append(out, p)
	}
	return out
}

func pctBytes(total uint64, p float64) uint64 {
	if total == 0 || p <= 0 {
		return 0
	}
	return uint64(float64(total) * p / 100)
}

// satMul 是饱和乘法：溢出时返回 uint64 上限而不是回绕。
// 历史回填的 Count / 总量都来自持久化 JSON，属不可信输入。
func satMul(a, b uint64) uint64 {
	if a == 0 || b == 0 {
		return 0
	}
	if a > ^uint64(0)/b {
		return ^uint64(0)
	}
	return a * b
}

// HistoryCounts 返回某节点各级别当前的桶数量（供测试与运维观测）。
func (s *Store) HistoryCounts(nodeID string) map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h := s.history[nodeID]
	if h == nil {
		return map[string]int{HistoryResolution1m: 0, HistoryResolution5m: 0, HistoryResolution1h: 0}
	}
	return map[string]int{
		HistoryResolution1m: h.m1.n,
		HistoryResolution5m: h.m5.n,
		HistoryResolution1h: h.h1.n,
	}
}

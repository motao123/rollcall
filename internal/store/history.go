package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
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

	historySchemaVersion = 1

	// HistorySaveInterval 是历史快照的落盘间隔。
	// 崩溃最多丢失该窗口的聚合点，原始 24h 会用于重启后回填。
	HistorySaveInterval = 5 * time.Minute
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
		nb := historyBucket{Index: idx}
		r.buf[r.head] = nb
		r.head = (r.head + 1) % r.size
		if r.n < r.size {
			r.n++
		} else {
			// 覆盖最旧桶后，逻辑起点随 head 前移。
		}
		b = &r.buf[(r.head-1+r.size)%r.size]
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
	if hasPrev {
		b.NetRx += counterDelta(prev.NetRx, m.NetRx)
		b.NetTx += counterDelta(prev.NetTx, m.NetTx)
	}
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
			return fmt.Errorf("历史快照损坏且无法隔离 %s: %w", s.historyPath, decodeErr)
		}
		log.Printf("历史快照损坏，已备份到 %s，将从 24h 原始数据重建: %v", bak, decodeErr)
		s.mu.Lock()
		s.rebuildHistoryFromRawLocked()
		s.mu.Unlock()
		return nil
	}
	if snap.SchemaVersion > historySchemaVersion {
		bak := s.historyPath + ".newer-" + time.Now().Format("20060102-150405")
		if renameErr := os.Rename(s.historyPath, bak); renameErr != nil {
			return fmt.Errorf("历史快照 schema 版本 %d 高于当前支持的 %d，且无法备份 %s: %w",
				snap.SchemaVersion, historySchemaVersion, s.historyPath, renameErr)
		}
		return fmt.Errorf("历史快照 schema 版本 %d 高于当前支持的 %d，已备份到 %s；请升级服务端",
			snap.SchemaVersion, historySchemaVersion, bak)
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
		r.appendLoaded(b)
	}
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
		p.MemUsed = pctBytes(b.MemTotal, p.MemPct)
		p.DiskUsed = pctBytes(b.DiskTotal, p.DiskPct)
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

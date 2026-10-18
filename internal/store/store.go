// Package store 实现内存时序存储与 gzip JSON 快照持久化。
//
// 设计取舍：不引入数据库依赖（对齐 Komari 的"零数据库"理念）。
// 40 台规模、30 秒粒度、保留 24 小时 = 每节点 2880 点，
// 全量常驻内存约 30MB，完全可控；定期快照到磁盘保证重启不丢历史。
package store

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/motao/rollcall/internal/model"
)

// RingSize 是每节点保留的采样点数。30s × 2880 = 24h。
const RingSize = 2880

// ServiceRingSize 是每服务保留的探测点数。
// 默认 60s 间隔时覆盖 48h，确保公开页/导出引用的「24h 可用率」有足够历史；
// 即使间隔调到 30s，也仍能覆盖 24h。
const ServiceRingSize = 2880

// OnlineWindow 内收到上报即视为在线。
const OnlineWindow = 120 // 秒

// StaleWindow 内仍有上报视为"数据延迟"：超过 OnlineWindow
// 但尚未达到真正离线。它只影响展示语义，告警仍按 OnlineWindow 判定。
const StaleWindow = 300 // 秒

// NodeState 根据最后上报时间推导展示状态。
// lastSeen <= 0 表示从未上报（no_data），与"曾在线后失联"区分开。
func NodeState(lastSeen, now int64) string {
	if lastSeen <= 0 {
		return model.NodeStateNoData
	}
	age := now - lastSeen
	switch {
	case age <= OnlineWindow:
		return model.NodeStateOnline
	case age <= StaleWindow:
		return model.NodeStateStale
	default:
		return model.NodeStateOffline
	}
}

// Ring 是定长环形缓冲，用于保存单节点时序。
type Ring struct {
	buf  []model.Metrics
	size int
	head int // 下一个写入位置
	n    int
}

func NewRing(size int) *Ring {
	return &Ring{buf: make([]model.Metrics, size), size: size}
}

func (r *Ring) Add(m model.Metrics) {
	// 同一秒内的重复上报覆盖，避免 Agent 重启产生重复点；
	// 时间倒退的点直接丢弃，否则 Slice/Since 的升序假设会被破坏。
	if r.n > 0 {
		last := (r.head - 1 + r.size) % r.size
		if r.buf[last].TS == m.TS {
			r.buf[last] = m
			return
		}
		if m.TS < r.buf[last].TS {
			return
		}
	}
	r.buf[r.head] = m
	r.head = (r.head + 1) % r.size
	if r.n < r.size {
		r.n++
	}
}

// Slice 返回按时间升序排列的副本。
func (r *Ring) Slice() []model.Metrics {
	out := make([]model.Metrics, 0, r.n)
	start := (r.head - r.n + r.size) % r.size
	for i := 0; i < r.n; i++ {
		out = append(out, r.buf[(start+i)%r.size])
	}
	return out
}

// Since 返回 ts 之后的点。
func (r *Ring) Since(ts int64) []model.Metrics {
	all := r.Slice()
	for i, p := range all {
		if p.TS >= ts {
			return all[i:]
		}
	}
	return nil
}

// Last 返回最新一点。
func (r *Ring) Last() (model.Metrics, bool) {
	if r.n == 0 {
		return model.Metrics{}, false
	}
	return r.buf[(r.head-1+r.size)%r.size], true
}

// Store 是全局状态容器。
type Store struct {
	mu       sync.RWMutex
	nodes    map[string]*model.Node
	hosts    map[string]model.HostInfo
	series   map[string]*Ring
	services map[string]*model.Service
	svcRes   map[string]*Ring
	rules    map[string]*model.AlertRule
	alerts   []*model.Alert
	settings map[string]string

	notifyJobs   map[string]*model.NotifyJob
	notifyStats  model.NotifyStats
	notifySilent map[string]*model.Silence // scope(global|ruleID) -> 静默期

	history       map[string]*nodeHistory
	historyPath   string
	historyDirty  bool
	historySaveMu sync.Mutex

	path    string
	dirty   bool
	loadErr error
	saveMu  sync.Mutex
	saveReq chan struct{}

	saveStatMu sync.Mutex
	saveStats  SaveStats
}

// SaveStats 是状态快照落盘的累计统计（进程生命周期内）。
type SaveStats struct {
	Count               int64   // 尝试次数（含失败）
	Failures            int64   // 失败次数
	LastDurationSeconds float64 // 最近一次成功落盘耗时
	LastSuccess         int64   // 最近一次成功落盘的 unix 时间
}

// New 创建并尝试从磁盘恢复状态。
//
// 快照损坏时不会静默以空状态启动（那会让全部节点与令牌凭空消失），
// 而是把损坏文件改名隔离，并通过 LoadErr 报告，由调用方决定是否退出。
func New(dataDir string) *Store {
	s := &Store{
		nodes:        map[string]*model.Node{},
		hosts:        map[string]model.HostInfo{},
		series:       map[string]*Ring{},
		services:     map[string]*model.Service{},
		svcRes:       map[string]*Ring{},
		rules:        map[string]*model.AlertRule{},
		settings:     map[string]string{},
		notifyJobs:   map[string]*model.NotifyJob{},
		notifySilent: map[string]*model.Silence{},
		history:      map[string]*nodeHistory{},
		path:         filepath.Join(dataDir, "state.json"),
		historyPath:  filepath.Join(dataDir, "history.json"),
		saveReq:      make(chan struct{}, 1),
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		s.loadErr = fmt.Errorf("创建数据目录 %s: %w", dataDir, err)
		return s
	}
	s.loadErr = s.load()
	if s.loadErr == nil {
		s.loadErr = s.loadHistory()
	}
	return s
}

// LoadErr 返回初始化时载入快照的错误（文件不存在不算错误）。
func (s *Store) LoadErr() error { return s.loadErr }

// ---------- 快照结构 ----------

// 快照以 gzip 压缩的 JSON 落盘：40 节点 × 2880 点的明文 JSON 约 30MB，
// 压缩后通常只有几 MB，可显著降低每分钟一次快照的磁盘写放大。
// 读取时按魔数识别，兼容升级前遗留的明文 JSON。
var gzipMagic = []byte{0x1f, 0x8b}

func encodeSnapshot(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decodeSnapshot(b []byte) ([]byte, error) {
	if !bytes.HasPrefix(b, gzipMagic) {
		return b, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("解压快照: %w", err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("解压快照: %w", err)
	}
	return raw, nil
}

// SnapshotSchemaVersion 是当前快照结构版本。
// 旧版快照没有该字段（解析为 0），仍按兼容格式载入；
// 高于当前版本的文件必须显式拒绝，避免新结构被旧程序静默读成零值。
//
// v3 仅提升版本号：历史聚合写入独立的 history.json，主快照结构未变。
const SnapshotSchemaVersion = 3

type snapshot struct {
	SchemaVersion int                         `json:"schemaVersion"`
	Nodes         map[string]*model.Node      `json:"nodes"`
	Hosts         map[string]model.HostInfo   `json:"hosts"`
	Series        map[string][]model.Metrics  `json:"series"`
	Services      map[string]*model.Service   `json:"services"`
	SvcRes        map[string][]model.Metrics  `json:"svcRes"`
	Rules         map[string]*model.AlertRule `json:"rules"`
	Alerts        []*model.Alert              `json:"alerts"`
	// Tokens 单独持久化：model.Node.Token 标了 json:"-"，
	// 不会随节点一起写入，否则重启后 Agent 全部认证失败。
	Tokens       map[string]string         `json:"tokens"`
	Settings     map[string]string         `json:"settings"`
	NotifyJobs   []*model.NotifyJob        `json:"notifyJobs,omitempty"`
	NotifyStats  model.NotifyStats         `json:"notifyStats"`
	NotifySilent map[string]*model.Silence `json:"notifySilences,omitempty"`
	SavedAt      int64                     `json:"savedAt"`
}

func (s *Store) load() error {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取快照 %s: %w", s.path, err)
	}
	raw, err := decodeSnapshot(b)
	var snap snapshot
	if err == nil {
		err = json.Unmarshal(raw, &snap)
	}
	if err != nil {
		bak := s.path + ".corrupt-" + time.Now().Format("20060102-150405")
		if renameErr := os.Rename(s.path, bak); renameErr != nil {
			// 隔离失败是当前要处理的故障，应包装 renameErr；
			// decodeErr 已通过返回值语义说明，混在 %w 里会让调用方的
			// errors.Is/As 误判。
			return fmt.Errorf("快照损坏且无法隔离 %s: %w", s.path, renameErr)
		}
		return fmt.Errorf("快照损坏，已备份到 %s: %w", bak, err)
	}
	if snap.SchemaVersion > SnapshotSchemaVersion {
		bak := s.path + ".newer-" + time.Now().Format("20060102-150405")
		if renameErr := os.Rename(s.path, bak); renameErr != nil {
			return fmt.Errorf("快照 schema 版本 %d 高于当前支持的 %d，且无法备份 %s: %w",
				snap.SchemaVersion, SnapshotSchemaVersion, s.path, renameErr)
		}
		return fmt.Errorf("快照 schema 版本 %d 高于当前支持的 %d，已备份到 %s；请升级服务端",
			snap.SchemaVersion, SnapshotSchemaVersion, bak)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range snap.Nodes {
		if v == nil {
			continue
		}
		if tok, ok := snap.Tokens[k]; ok {
			v.Token = tok // 还原上报令牌
		}
		s.nodes[k] = v
	}
	for k, v := range snap.Hosts {
		s.hosts[k] = v
	}
	for k, pts := range snap.Series {
		sort.Slice(pts, func(i, j int) bool { return pts[i].TS < pts[j].TS })
		r := NewRing(RingSize)
		for _, p := range pts {
			r.Add(p)
		}
		s.series[k] = r
	}
	for k, v := range snap.Services {
		if v == nil {
			continue
		}
		s.services[k] = v
	}
	for k, pts := range snap.SvcRes {
		sort.Slice(pts, func(i, j int) bool { return pts[i].TS < pts[j].TS })
		r := NewRing(ServiceRingSize)
		for _, p := range pts {
			r.Add(p)
		}
		s.svcRes[k] = r
	}
	for k, v := range snap.Rules {
		if v == nil {
			continue
		}
		s.rules[k] = v
	}
	for _, a := range snap.Alerts {
		if a != nil {
			s.alerts = append(s.alerts, a)
		}
	}
	for _, j := range snap.NotifyJobs {
		if j == nil || j.ID == "" {
			continue
		}
		s.notifyJobs[j.ID] = j
	}
	s.notifyStats = snap.NotifyStats
	s.notifyStats.Pending = 0 // Pending 由当前任务实时计算，不信任快照值
	for scope, sl := range snap.NotifySilent {
		if sl != nil && scope != "" {
			s.notifySilent[scope] = sl
		}
	}
	if snap.Settings != nil {
		s.settings = snap.Settings
	}
	return nil
}

// Save 将内存状态写入磁盘。
//
// ★ 必须在持锁期间完成**深拷贝**，再于锁外序列化。
// 早期实现把 s.nodes / s.hosts 等 map 的引用直接塞进快照，
// 解锁后才 json.Marshal —— 而 marshal 会迭代这些 map，
// 与并发的 Agent 上报（AddMetrics 写 hosts）、管理 API（UpsertNode/
// UpsertRule/SetSetting）撞车，触发 Go runtime 的
// "fatal error: concurrent map iteration and map write"。
// 该错误**不可 recover**，整个进程直接死亡；而 StartAutoSave 每 60 秒
// 快照一次、40 节点每 30 秒上报一次，撞车是必然而非偶然。
func (s *Store) Save() (err error) {
	start := time.Now()
	defer func() { s.recordSave(time.Since(start), err) }()

	// ★ saveMu 必须在抓快照之前获取。
	// 若先抓快照、序列化完才拿 saveMu，两个并发 Save 可能
	// 「先抓旧状态、后抓新状态」却按相反顺序落盘，旧快照覆盖新快照：
	// 典型后果是刚轮换的节点令牌被抹掉，重启后 Agent 全部 401。
	// 锁顺序固定为 saveMu → s.mu；Save 的调用方都不持有 s.mu。
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	s.mu.RLock()
	snap := snapshot{
		SchemaVersion: SnapshotSchemaVersion,
		Nodes:         make(map[string]*model.Node, len(s.nodes)),
		Hosts:         make(map[string]model.HostInfo, len(s.hosts)),
		Services:      make(map[string]*model.Service, len(s.services)),
		Rules:         make(map[string]*model.AlertRule, len(s.rules)),
		Alerts:        make([]*model.Alert, 0, len(s.alerts)),
		Series:        make(map[string][]model.Metrics, len(s.series)),
		SvcRes:        make(map[string][]model.Metrics, len(s.svcRes)),
		Tokens:        make(map[string]string, len(s.nodes)),
		Settings:      make(map[string]string, len(s.settings)),
		NotifyJobs:    make([]*model.NotifyJob, 0, len(s.notifyJobs)),
		NotifyStats:   s.notifyStats,
		NotifySilent:  make(map[string]*model.Silence, len(s.notifySilent)),
		SavedAt:       time.Now().Unix(),
	}
	// 以下均为值类型或纯值字段结构体，解引用即得到独立副本
	for k, n := range s.nodes {
		cp := *n
		snap.Nodes[k] = &cp
		if n.Token != "" {
			snap.Tokens[k] = n.Token
		}
	}
	for k, v := range s.hosts {
		snap.Hosts[k] = v
	}
	for k, v := range s.services {
		cp := *v
		snap.Services[k] = &cp
	}
	for k, v := range s.rules {
		cp := cloneRule(v)
		snap.Rules[k] = &cp
	}
	for _, a := range s.alerts {
		cp := *a
		snap.Alerts = append(snap.Alerts, &cp)
	}
	for k, v := range s.settings {
		snap.Settings[k] = v
	}
	for _, j := range s.notifyJobs {
		cp := *j
		snap.NotifyJobs = append(snap.NotifyJobs, &cp)
	}
	sort.Slice(snap.NotifyJobs, func(i, j int) bool {
		if snap.NotifyJobs[i].CreatedAt != snap.NotifyJobs[j].CreatedAt {
			return snap.NotifyJobs[i].CreatedAt < snap.NotifyJobs[j].CreatedAt
		}
		return snap.NotifyJobs[i].ID < snap.NotifyJobs[j].ID
	})
	for scope, sl := range s.notifySilent {
		cp := *sl
		snap.NotifySilent[scope] = &cp
	}
	for k, r := range s.series {
		snap.Series[k] = r.Slice()
	}
	for k, r := range s.svcRes {
		snap.SvcRes[k] = r.Slice()
	}
	s.mu.RUnlock()

	b, err := json.Marshal(&snap)
	if err != nil {
		return err
	}
	b, err = encodeSnapshot(b)
	if err != nil {
		return fmt.Errorf("压缩快照: %w", err)
	}
	// 写入已被函数开头的 saveMu 串行化：固定 temp 路径下，
	// 两个并发 Save 会互相截断/覆盖。
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// recordSave 汇总落盘耗时，供 /metrics 与运维排查使用。
// 失败时只累计次数，保留上一次成功的耗时作为参考。
func (s *Store) recordSave(d time.Duration, err error) {
	s.saveStatMu.Lock()
	defer s.saveStatMu.Unlock()
	s.saveStats.Count++
	if err != nil {
		s.saveStats.Failures++
		return
	}
	s.saveStats.LastDurationSeconds = d.Seconds()
	s.saveStats.LastSuccess = time.Now().Unix()
}

// SaveStats 返回落盘统计的副本。
func (s *Store) SaveStats() SaveStats {
	s.saveStatMu.Lock()
	defer s.saveStatMu.Unlock()
	return s.saveStats
}

// StartAutoSave 启动后台快照。
//
// 除固定间隔外，还监听 RequestSave：通知入队、投递结果、静默变更等
// 关键状态会请求尽快落盘，避免重启后丢任务。请求按 2 秒窗口合并，
// 告警风暴时不会对每个任务各做一次全量快照。
func (s *Store) StartAutoSave(every time.Duration) {
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for range t.C {
			s.saveLogged()
		}
	}()
	go func() {
		for range s.saveReq {
			time.Sleep(2 * time.Second)
			// 合并窗口内到达的后续请求；本窗口之后的请求留给下一轮。
		drain:
			for {
				select {
				case <-s.saveReq:
				default:
					break drain
				}
			}
			s.saveLogged()
		}
	}()
}

// RequestSave 请求尽快落盘。非阻塞，可安全用于高频路径。
func (s *Store) RequestSave() {
	select {
	case s.saveReq <- struct{}{}:
	default:
	}
}

func (s *Store) saveLogged() {
	if err := s.Save(); err != nil {
		log.Printf("自动保存失败: %v", err)
	}
}

// ---------- 节点 ----------

func NewID(prefix string) string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand 失败: " + err.Error())
	}
	return prefix + "_" + hex.EncodeToString(b)
}

// NewToken 生成节点上报令牌（24 字节随机，hex 编码）。
func NewToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand 失败: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func (s *Store) UpsertNode(n *model.Node) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.nodes[n.ID]; !ok {
		if s.series[n.ID] == nil {
			s.series[n.ID] = NewRing(RingSize)
		}
	}
	s.nodes[n.ID] = n
}

// DeleteNode 删除节点及其监控数据，并关闭其所有未恢复的告警。
//
// 告警必须一并关闭：否则被删节点的活跃告警会永远挂在「进行中」，
// 而且它已不在 Nodes() 里，Evaluate 的恢复分支永远走不到 —— 成为无法自愈的僵尸告警。
func (s *Store) DeleteNode(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.nodes, id)
	delete(s.series, id)
	delete(s.hosts, id)
	if _, ok := s.history[id]; ok {
		delete(s.history, id)
		s.historyDirty = true
	}
	now := time.Now().Unix()
	for _, a := range s.alerts {
		if a.NodeID == id && a.ResolvedAt == 0 {
			a.ResolvedAt = now
		}
	}
	for _, j := range s.notifyJobs {
		if j.NodeID == id && j.Status == model.NotifyPending {
			j.Status = model.NotifySuppressed
			j.SuppressedBy = "node:" + id
			j.LastError = "节点已删除"
			j.NextAttemptAt = 0
			s.notifyStats.Suppressed++
		}
	}
	s.RequestSave()
}

func (s *Store) Nodes() []*model.Node {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*model.Node, 0, len(s.nodes))
	for _, n := range s.nodes {
		cp := *n
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Weight != out[j].Weight {
			return out[i].Weight < out[j].Weight
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (s *Store) Node(id string) (*model.Node, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n, ok := s.nodes[id]
	if !ok {
		return nil, false
	}
	cp := *n
	return &cp, true
}

func (s *Store) NodeByToken(token, id string) (*model.Node, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n, ok := s.nodes[id]
	if !ok || n.Token == "" || token == "" {
		return nil, false
	}
	// 常量时间比较，避免通过响应耗时逐字节猜测令牌
	if subtle.ConstantTimeCompare([]byte(n.Token), []byte(token)) != 1 {
		return nil, false
	}
	cp := *n
	return &cp, true
}

// cloneRule 深拷贝告警规则。
//
// AlertRule 是唯一带引用字段（NodeIDs / Groups 切片）的持久化结构。
// 只写 `cp := *v` 会共享底层数组：快照在锁外序列化时，若处理器在
// UpsertRule 之后继续 append 该切片（还有空闲容量），就会与 json.Marshal
// 的迭代撞车，甚至把后续修改写回已入快照的对象。
func cloneRule(r *model.AlertRule) model.AlertRule {
	cp := *r
	if r.NodeIDs != nil {
		cp.NodeIDs = append([]string(nil), r.NodeIDs...)
	}
	if r.Groups != nil {
		cp.Groups = append([]string(nil), r.Groups...)
	}
	return cp
}

// ---------- 指标 ----------

func (s *Store) AddMetrics(nodeID string, host model.HostInfo, m model.Metrics) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.series[nodeID]
	if r == nil {
		r = NewRing(RingSize)
		s.series[nodeID] = r
	}
	// 主机静态信息先于去重逻辑更新：同一秒内的重复上报同样可能带上
	// 新的 hostname/内核版本，原来那条 return 会让它永远写不进去。
	if host.Hostname != "" {
		s.hosts[nodeID] = host
	}
	var prev model.Metrics
	hasPrev := false
	if r.n > 0 {
		prev, hasPrev = r.Last()
		// 同一秒重复上报会覆盖原始点；历史已计入上一份数据，
		// 若再聚合一次会造成平均值与流量重复累计。
		if m.TS <= prev.TS {
			r.Add(m)
			return
		}
	}
	r.Add(m)
	s.addHistoryLocked(nodeID, m, &prev, hasPrev)
}

func (s *Store) Host(nodeID string) model.HostInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hosts[nodeID]
}

func (s *Store) Latest(nodeID string) (model.Metrics, int64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r := s.series[nodeID]
	if r == nil {
		return model.Metrics{}, 0, false
	}
	m, ok := r.Last()
	return m, m.TS, ok
}

func (s *Store) Series(nodeID string, since int64) []model.Metrics {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r := s.series[nodeID]
	if r == nil {
		return nil
	}
	if since <= 0 {
		return r.Slice()
	}
	return r.Since(since)
}

// ---------- 服务 ----------

func (s *Store) UpsertService(sv *model.Service) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.svcRes[sv.ID] == nil {
		s.svcRes[sv.ID] = NewRing(ServiceRingSize)
	}
	s.services[sv.ID] = sv
}

func (s *Store) BackupServices() []*model.Service {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*model.Service, 0, len(s.services))
	for _, v := range s.services {
		cp := *v
		out = append(out, &cp)
	}
	return out
}

func (s *Store) Services() []*model.Service {
	out := s.BackupServices()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// AddServiceResult 记录探测结果（独立于指标的轻量环形缓冲）。
func (s *Store) AddServiceResult(sr model.ServiceResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.svcRes[sr.ServiceID]
	if r == nil {
		r = NewRing(ServiceRingSize)
		s.svcRes[sr.ServiceID] = r
	}
	// 复用 Metrics.TS 存时间戳，CPUPct 存延迟（压缩存储）
	r.Add(model.Metrics{TS: sr.TS, CPUPct: float64(sr.LatencyMs), TCPConn: b2i(sr.OK), Procs: sr.CertDays})
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ServiceSeries 返回探测历史（时间, 延迟毫秒, 是否成功）。
func (s *Store) ServiceSeries(id string) [][3]int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r := s.svcRes[id]
	if r == nil {
		return nil
	}
	all := r.Slice()
	out := make([][3]int64, 0, len(all))
	for _, p := range all {
		out = append(out, [3]int64{p.TS, int64(p.CPUPct), int64(p.TCPConn)})
	}
	return out
}

// ServiceLast 返回最近一次探测结果的摘要。
func (s *Store) ServiceLast(id string) (ts int64, latency int, ok bool, has bool) {
	ts, latency, ok, _, has = s.ServiceLastFull(id)
	return
}

// ServiceLastFull 额外返回 TLS 证书剩余天数（非 https 为 -1）。
func (s *Store) ServiceLastFull(id string) (ts int64, latency int, ok bool, certDays int, has bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r := s.svcRes[id]
	if r == nil {
		return 0, 0, false, -1, false
	}
	p, ok2 := r.Last()
	if !ok2 {
		return 0, 0, false, -1, false
	}
	return p.TS, int(p.CPUPct), p.TCPConn == 1, p.Procs, true
}

// ---------- 告警 ----------

func (s *Store) UpsertRule(r *model.AlertRule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules[r.ID] = r
}

func (s *Store) Rules() []*model.AlertRule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*model.AlertRule, 0, len(s.rules))
	for _, v := range s.rules {
		cp := cloneRule(v)
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Store) Alerts(limit int) []*model.Alert {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*model.Alert, 0, len(s.alerts))
	for i := len(s.alerts) - 1; i >= 0; i-- {
		cp := *s.alerts[i]
		out = append(out, &cp)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// ActiveAlerts 返回所有未恢复告警的副本（用于引擎重启后恢复告警状态）。
func (s *Store) ActiveAlerts() []*model.Alert {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*model.Alert, 0)
	for _, a := range s.alerts {
		if a.ResolvedAt == 0 {
			cp := *a
			out = append(out, &cp)
		}
	}
	return out
}

// ActiveAlertsByRule 返回某规则名下未恢复告警的副本。
// 删除规则前先取快照，删除后再据此补发恢复通知。
func (s *Store) ActiveAlertsByRule(ruleID string) []*model.Alert {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*model.Alert, 0)
	for _, a := range s.alerts {
		if a.RuleID == ruleID && a.ResolvedAt == 0 {
			cp := *a
			out = append(out, &cp)
		}
	}
	return out
}

// OpenAlert 断言/维持一条告警；返回是否为新建、以及节点与规则是否仍然存在。
//
// 存在性必须与写入在同一把锁内确认：否则「评估时节点还在、写入前被删除」
// 会留下一条永远无法自愈的未恢复告警。
func (s *Store) OpenAlert(a *model.Alert) (created, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.ResolvedAt == 0 {
		if _, exists := s.nodes[a.NodeID]; !exists {
			return false, false
		}
		if _, exists := s.rules[a.RuleID]; !exists {
			return false, false
		}
	}
	for _, ex := range s.alerts {
		if ex.RuleID == a.RuleID && ex.NodeID == a.NodeID && ex.ResolvedAt == 0 {
			ex.Value = a.Value
			ex.Message = a.Message
			// 回填持久化告警的真实 ID，通知任务据此关联告警记录。
			a.ID = ex.ID
			// 值/文案已就地改写，必须落盘：否则重启后「当前值」回到旧数字。
			s.RequestSave()
			return false, true
		}
	}
	s.alerts = append(s.alerts, a)
	if len(s.alerts) > 500 {
		// 只淘汰已恢复的旧记录：未恢复告警必须保留，
		// 否则会把仍在进行的告警从历史里抹掉。
		kept := make([]*model.Alert, 0, 500)
		unresolved := make([]*model.Alert, 0, len(s.alerts))
		for _, ex := range s.alerts {
			if ex.ResolvedAt == 0 {
				unresolved = append(unresolved, ex)
			}
		}
		kept = append(kept, unresolved...)
		for i := len(s.alerts) - 1; i >= 0 && len(kept) < 500; i-- {
			if s.alerts[i].ResolvedAt != 0 {
				kept = append(kept, s.alerts[i])
			}
		}
		sort.Slice(kept, func(i, j int) bool { return kept[i].StartedAt < kept[j].StartedAt })
		s.alerts = kept
	}
	// 新告警必须落盘：否则重启后「进行中」的告警全部消失，
	// 恢复通知也无从发出。
	s.RequestSave()
	return true, true
}

// ResolveAlert 关闭匹配的未恢复告警。
// ResolveAlert 关闭指定规则/节点的未恢复告警，返回是否确实关闭了记录。
// 返回值用于区分「内存追踪状态丢失」与「持久化告警仍在进行中」，
// 让重启后的恢复通知也能正常发出。
func (s *Store) ResolveAlert(ruleID, nodeID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	resolved := false
	for _, a := range s.alerts {
		if a.RuleID == ruleID && a.NodeID == nodeID && a.ResolvedAt == 0 {
			a.ResolvedAt = now
			resolved = true
		}
	}
	if resolved {
		// 恢复状态必须落盘，否则重启后已恢复的告警会「复活」。
		s.RequestSave()
	}
	return resolved
}

// ActiveAlertCount 统计未恢复告警数。
func (s *Store) ActiveAlertCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, a := range s.alerts {
		if a.ResolvedAt == 0 {
			n++
		}
	}
	return n
}

// ActiveAlertsByNode 统计每节点未恢复告警数。
func (s *Store) ActiveAlertsByNode() map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := map[string]int{}
	for _, a := range s.alerts {
		if a.ResolvedAt == 0 {
			m[a.NodeID]++
		}
	}
	return m
}

// ---------- 服务删除 ----------

// DeleteService 删除探测目标及其历史。
func (s *Store) DeleteService(id string) bool {
	s.mu.Lock()
	if _, ok := s.services[id]; !ok {
		s.mu.Unlock()
		return false
	}
	delete(s.services, id)
	delete(s.svcRes, id)
	s.mu.Unlock()
	// 变更已进入内存，必须请求落盘：否则重启后删除的服务会「复活」。
	s.RequestSave()
	return true
}

// ---------- 键值设置 ----------

// SetSetting 保存一段配置（JSON 字符串）。
func (s *Store) SetSetting(key, value string) {
	s.mu.Lock()
	if s.settings == nil {
		s.settings = map[string]string{}
	}
	changed := s.settings[key] != value
	s.settings[key] = value
	s.mu.Unlock()
	// 设置项（主题、通知开关等）必须主动请求落盘：
	// 只靠 60 秒自动快照时，「保存后立刻重启」会让设置静默回退。
	if changed {
		s.RequestSave()
	}
}

// GetSetting 读取配置。
func (s *Store) GetSetting(key string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.settings[key]
}

// DeleteRule 删除告警规则，并关闭其所有未恢复的告警。
func (s *Store) DeleteRule(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rules[id]; !ok {
		return false
	}
	delete(s.rules, id)
	delete(s.notifySilent, id)
	now := time.Now().Unix()
	for _, a := range s.alerts {
		if a.RuleID == id && a.ResolvedAt == 0 {
			a.ResolvedAt = now
		}
	}
	// 规则已删除，尚未投递的通知不再有意义，避免管理员收到已撤销规则的告警。
	for _, j := range s.notifyJobs {
		if j.RuleID == id && j.Status == model.NotifyPending {
			j.Status = model.NotifySuppressed
			j.SuppressedBy = id
			j.LastError = "规则已删除"
			j.NextAttemptAt = 0
			s.notifyStats.Suppressed++
		}
	}
	s.RequestSave()
	return true
}

// ResolveRuleAlerts 关闭某规则名下所有未恢复的告警，返回被关闭告警的副本。
// 规则被停用或删除时调用，避免遗留永远无法自愈的「进行中」告警：
// 引擎只对启用中的规则跑恢复分支，停用规则的告警再没有别的关闭路径。
func (s *Store) ResolveRuleAlerts(ruleID string) []*model.Alert {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	var closed []*model.Alert
	for _, a := range s.alerts {
		if a.RuleID == ruleID && a.ResolvedAt == 0 {
			a.ResolvedAt = now
			cp := *a
			closed = append(closed, &cp)
		}
	}
	if len(closed) > 0 {
		// 与 ResolveAlert 同理：不落盘的话重启后这批告警会重新变成「进行中」，
		// 而引擎已经不会为停用/删除的规则跑恢复分支，形成永不自愈的僵尸告警。
		s.RequestSave()
	}
	return closed
}

// ---------- 通知队列与静默 ----------

const (
	// MaxNotifyJobs 限制内存/快照中的任务总数，防止异常场景无限增长。
	MaxNotifyJobs = 2000
	// MaxFinalNotifyJobs 是保留的已结束任务数（sent/failed/suppressed）。
	MaxFinalNotifyJobs = 500
	// NotifyJobRetention 是已结束任务的保留时长。
	NotifyJobRetention = 7 * 24 * time.Hour
	// NotifyLease 是任务被领取后的租约：进程崩溃时任务在租约到期后重新变为可投递。
	NotifyLease = 5 * time.Minute
)

// EnqueueNotifyJob 写入一条投递任务。队列已满且无法清理时返回 false。
// 任务状态为 suppressed 时会同步累计静默计数（用于入队即静默的场景）。
func (s *Store) EnqueueNotifyJob(j *model.NotifyJob) bool {
	s.mu.Lock()
	if j.ID == "" || s.notifyJobs[j.ID] != nil {
		s.mu.Unlock()
		return false
	}
	if len(s.notifyJobs) >= MaxNotifyJobs {
		s.pruneNotifyLocked(time.Now().Unix())
	}
	if len(s.notifyJobs) >= MaxNotifyJobs {
		s.notifyStats.Rejected++
		s.mu.Unlock()
		log.Printf("通知队列已满（%d 条），拒绝新任务: %s", MaxNotifyJobs, j.Title)
		return false
	}
	cp := *j
	if cp.Status == model.NotifySuppressed {
		s.notifyStats.Suppressed++
	}
	s.notifyJobs[cp.ID] = &cp
	s.mu.Unlock()
	s.RequestSave()
	return true
}

// ClaimNotifyJobs 领取到期的待投递任务，并把 NextAttemptAt 推到租约到期时间，
// 避免多轮调度或进程内重复投递。返回的是副本。
func (s *Store) ClaimNotifyJobs(now int64, limit int) []*model.NotifyJob {
	if limit <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	due := make([]*model.NotifyJob, 0)
	for _, j := range s.notifyJobs {
		if j.Status == model.NotifyPending && j.NextAttemptAt <= now {
			due = append(due, j)
		}
	}
	sort.Slice(due, func(i, k int) bool {
		if due[i].NextAttemptAt != due[k].NextAttemptAt {
			return due[i].NextAttemptAt < due[k].NextAttemptAt
		}
		return due[i].CreatedAt < due[k].CreatedAt
	})
	if len(due) > limit {
		due = due[:limit]
	}
	out := make([]*model.NotifyJob, 0, len(due))
	leaseUntil := now + int64(NotifyLease/time.Second)
	for _, j := range due {
		j.NextAttemptAt = leaseUntil
		cp := *j
		out = append(out, &cp)
	}
	return out
}

// NextNotifyAt 返回最近一条待投递任务的计划时间。
func (s *Store) NextNotifyAt() (int64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var next int64
	found := false
	for _, j := range s.notifyJobs {
		if j.Status != model.NotifyPending {
			continue
		}
		if !found || j.NextAttemptAt < next {
			next = j.NextAttemptAt
			found = true
		}
	}
	return next, found
}

// NotifyJobSent 标记投递成功（任一通道成功即算送达，避免重试造成重复刷屏）。
func (s *Store) NotifyJobSent(id, lastError string, now int64) bool {
	s.mu.Lock()
	j, ok := s.notifyJobs[id]
	if !ok || j.Status != model.NotifyPending {
		s.mu.Unlock()
		return false
	}
	j.Attempts++
	j.Status = model.NotifySent
	j.SentAt = now
	j.LastError = lastError
	j.NextAttemptAt = 0
	s.notifyStats.Sent++
	s.notifyStats.LastSuccess = now
	s.mu.Unlock()
	s.RequestSave()
	return true
}

// NotifyJobRetry 标记本轮失败并安排下一次重试。
func (s *Store) NotifyJobRetry(id, lastError string, nextAt, now int64) bool {
	s.mu.Lock()
	j, ok := s.notifyJobs[id]
	if !ok || j.Status != model.NotifyPending {
		s.mu.Unlock()
		return false
	}
	j.Attempts++
	j.LastError = lastError
	j.NextAttemptAt = nextAt
	j.Status = model.NotifyPending
	s.notifyStats.Retries++
	s.mu.Unlock()
	s.RequestSave()
	return true
}

// NotifyJobFailed 标记重试耗尽的最终失败。
func (s *Store) NotifyJobFailed(id, lastError string, now int64) bool {
	s.mu.Lock()
	j, ok := s.notifyJobs[id]
	if !ok || j.Status != model.NotifyPending {
		s.mu.Unlock()
		return false
	}
	j.Attempts++
	j.Status = model.NotifyFailed
	j.LastError = lastError
	j.NextAttemptAt = 0
	s.notifyStats.Failed++
	s.mu.Unlock()
	s.RequestSave()
	return true
}

// NotifyJobSuppressed 标记任务因静默或配置停用未投递。
func (s *Store) NotifyJobSuppressed(id, by, reason string, now int64) bool {
	s.mu.Lock()
	j, ok := s.notifyJobs[id]
	if !ok || j.Status != model.NotifyPending {
		s.mu.Unlock()
		return false
	}
	j.Status = model.NotifySuppressed
	j.SuppressedBy = by
	j.LastError = reason
	j.NextAttemptAt = 0
	s.notifyStats.Suppressed++
	s.mu.Unlock()
	s.RequestSave()
	return true
}

var (
	ErrNotifyJobNotFound = errors.New("通知任务不存在")
	ErrNotifyJobPending  = errors.New("通知任务已在队列中")
	ErrNotifyJobSent     = errors.New("通知任务已投递成功")
)

// NotifyJobs 返回最近的通知任务副本，未完成与失败任务排在前面。
func (s *Store) NotifyJobs(limit int) []*model.NotifyJob {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*model.NotifyJob, 0, len(s.notifyJobs))
	for _, j := range s.notifyJobs {
		cp := *j
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, k int) bool {
		ri, rk := notifyStatusRank(out[i].Status), notifyStatusRank(out[k].Status)
		if ri != rk {
			return ri < rk
		}
		if out[i].CreatedAt != out[k].CreatedAt {
			return out[i].CreatedAt > out[k].CreatedAt
		}
		return out[i].ID > out[k].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func notifyStatusRank(status string) int {
	switch status {
	case model.NotifyPending:
		return 0
	case model.NotifyFailed:
		return 1
	case model.NotifySuppressed:
		return 2
	default:
		return 3
	}
}

// RetryNotifyJob 把失败或被拦截的任务重新放回队列，并重置尝试次数。
func (s *Store) RetryNotifyJob(id string, now int64) error {
	s.mu.Lock()
	j, ok := s.notifyJobs[id]
	if !ok {
		s.mu.Unlock()
		return ErrNotifyJobNotFound
	}
	switch j.Status {
	case model.NotifyPending:
		s.mu.Unlock()
		return ErrNotifyJobPending
	case model.NotifySent:
		s.mu.Unlock()
		return ErrNotifyJobSent
	}
	j.Status = model.NotifyPending
	j.Attempts = 0
	j.LastError = ""
	j.SuppressedBy = ""
	j.NextAttemptAt = now
	s.mu.Unlock()
	s.RequestSave()
	return nil
}

// NotifyStats 返回队列深度与累计投递统计。
func (s *Store) NotifyStats() model.NotifyStats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := s.notifyStats
	for _, j := range s.notifyJobs {
		if j.Status == model.NotifyPending {
			st.Pending++
		}
	}
	return st
}

// PruneNotify 清理过期/超量的已结束任务与过期静默，不影响未完成任务。
func (s *Store) PruneNotify(now int64) {
	s.mu.Lock()
	s.pruneNotifyLocked(now)
	s.mu.Unlock()
}

func (s *Store) pruneNotifyLocked(now int64) {
	retain := int64(NotifyJobRetention / time.Second)
	for id, j := range s.notifyJobs {
		if j.Status == model.NotifyPending {
			continue
		}
		ts := j.CreatedAt
		if j.SentAt > ts {
			ts = j.SentAt
		}
		if ts > 0 && now-ts > retain {
			delete(s.notifyJobs, id)
		}
	}
	// 已结束任务按时间倒序保留最近 N 条；未完成任务永不淘汰。
	final := make([]*model.NotifyJob, 0, len(s.notifyJobs))
	for _, j := range s.notifyJobs {
		if j.Status != model.NotifyPending {
			final = append(final, j)
		}
	}
	if len(final) > MaxFinalNotifyJobs {
		sort.Slice(final, func(i, k int) bool {
			if final[i].CreatedAt != final[k].CreatedAt {
				return final[i].CreatedAt > final[k].CreatedAt
			}
			return final[i].ID > final[k].ID
		})
		for _, j := range final[MaxFinalNotifyJobs:] {
			delete(s.notifyJobs, j.ID)
		}
	}
	for scope, sl := range s.notifySilent {
		if sl.Until > 0 && sl.Until <= now {
			delete(s.notifySilent, scope)
		}
	}
}

// SetSilence 新增或覆盖某个范围（global / 规则 ID）的静默期。
func (s *Store) SetSilence(sl *model.Silence) {
	s.mu.Lock()
	s.notifySilent[sl.Scope] = &model.Silence{
		Scope: sl.Scope, Until: sl.Until, Reason: sl.Reason, CreatedAt: sl.CreatedAt,
	}
	s.mu.Unlock()
	s.RequestSave()
}

// DeleteSilence 取消某个范围的静默，返回是否存在。
func (s *Store) DeleteSilence(scope string) bool {
	s.mu.Lock()
	_, ok := s.notifySilent[scope]
	delete(s.notifySilent, scope)
	s.mu.Unlock()
	if ok {
		s.RequestSave()
	}
	return ok
}

// Silences 返回当前仍生效的静默期。
func (s *Store) Silences(now int64) []*model.Silence {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*model.Silence, 0, len(s.notifySilent))
	for _, sl := range s.notifySilent {
		if sl.Until <= now {
			continue
		}
		cp := *sl
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		return out[i].Until < out[j].Until
	})
	return out
}

// SilenceFor 返回规则在指定时间生效的静默范围：全局优先，其次规则级。
func (s *Store) SilenceFor(ruleID string, now int64) (scope, reason string, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if sl, exists := s.notifySilent["global"]; exists && sl.Until > now {
		return "global", sl.Reason, true
	}
	if ruleID != "" {
		if sl, exists := s.notifySilent[ruleID]; exists && sl.Until > now {
			return ruleID, sl.Reason, true
		}
	}
	return "", "", false
}

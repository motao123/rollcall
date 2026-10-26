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
	"strings"

	"github.com/motao123/rollcall/internal/agentwire"
	"sort"
	"sync"
	"time"

	"github.com/motao123/rollcall/internal/model"
)

// RingSize 是每节点保留的采样点数。30s × 2880 = 24h。
const RingSize = 2880

// ServiceRingSize 是每服务保留的探测点数。
// 默认 60s 间隔时覆盖 48h，确保公开页/导出引用的「24h 可用率」有足够历史；
// 即使间隔调到 30s，也仍能覆盖 24h。
const ServiceRingSize = 2880

// OnlineWindow 内收到上报即视为在线。
//
// 取值来自 internal/agentwire：它是 Agent 与服务端共用的上报契约，
// Agent 的上报间隔上限由它推出。两侧各写一份数字迟早漂移，表现为
// 「配置看着合法、实际上必然误报失联」——只在网络抖动时暴露，极难复现。
const OnlineWindow = agentwire.OnlineTTL

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

// Ring 是定长环形缓冲，用于保存单节点时序与服务探测序列。
//
// 泛型化是因为两种序列的元素类型不同（节点指标 / 探测采样）。
// 时间戳怎么取由构造时的 at 提供，而不是让元素类型去实现某个接口 ——
// 元素是 model 包里的纯数据结构，不该为了存储细节反向依赖 store。
type Ring[T any] struct {
	buf  []T
	size int
	head int // 下一个写入位置
	n    int
	at   func(T) int64
	zero T
}

func newRing[T any](size int, at func(T) int64) *Ring[T] {
	if size <= 0 {
		size = 1
	}
	return &Ring[T]{buf: make([]T, size), size: size, at: at}
}

// NewRing 是节点指标序列的构造函数。
func NewRing(size int) *Ring[model.Metrics] {
	return newRing(size, func(m model.Metrics) int64 { return m.TS })
}

// NewServiceRing 是服务探测序列的构造函数。
func NewServiceRing(size int) *Ring[model.ServiceSample] {
	return newRing(size, func(s model.ServiceSample) int64 { return s.TS })
}

func (r *Ring[T]) Add(v T) {
	ts := r.at(v)
	// 同一秒内的重复上报覆盖，避免 Agent 重启产生重复点；
	// 时间倒退的点直接丢弃，否则 Slice/Since 的升序假设会被破坏。
	if r.n > 0 {
		last := (r.head - 1 + r.size) % r.size
		if r.at(r.buf[last]) == ts {
			r.buf[last] = v
			return
		}
		if ts < r.at(r.buf[last]) {
			return
		}
	}
	r.buf[r.head] = v
	r.head = (r.head + 1) % r.size
	if r.n < r.size {
		r.n++
	}
}

// Slice 返回按时间升序排列的副本。
func (r *Ring[T]) Slice() []T {
	out := make([]T, 0, r.n)
	start := (r.head - r.n + r.size) % r.size
	for i := 0; i < r.n; i++ {
		out = append(out, r.buf[(start+i)%r.size])
	}
	return out
}

// Since 返回 ts 之后的点。
func (r *Ring[T]) Since(ts int64) []T {
	all := r.Slice()
	for i, p := range all {
		if r.at(p) >= ts {
			return all[i:]
		}
	}
	return nil
}

// Last 返回最新一点。
func (r *Ring[T]) Last() (T, bool) {
	if r.n == 0 {
		return r.zero, false
	}
	return r.buf[(r.head-1+r.size)%r.size], true
}

// Store 是全局状态容器。
type Store struct {
	mu       sync.RWMutex
	nodes    map[string]*model.Node
	hosts    map[string]model.HostInfo
	series   map[string]*Ring[model.Metrics]
	services map[string]*model.Service
	svcRes   map[string]*Ring[model.ServiceSample]
	rules    map[string]*model.AlertRule
	alerts   []*model.Alert
	settings map[string]string

	// dropped 记录"刚被删除的节点 ID"，用于拦住在途上报。
	// 只在内存里：在途窗口是毫秒级，重启后不可能还有旧上报在路上。
	dropped map[string]int64

	// nicResetPending 记录"网卡集合刚变化、下一条指标要重建流量基线"的节点。
	// 与 handleNICSetLocked（置位）和 updateTrafficLocked（消费）在同一个
	// AddMetrics 临界区里完成，不落盘——节点上 NICSet 已更新，重启后下一次
	// 上报对比的仍是新值，不存在跨重启丢失的窗口。
	nicResetPending map[string]bool

	// 历史分片（data/series/<id>.json.gz）的增量写状态。
	// writeSeq 是全局单调写序号；seriesSeq[id] 记录该节点最后一次写入的序号，
	// seriesSaved[id] 记录已成功落盘的序号。Save 只写 seq > saved 的节点 ——
	// 30 秒上报 + 60 秒保存下，每次通常只有一半节点需要写。
	writeSeq     uint64
	seriesSeq    map[string]uint64
	seriesSaved  map[string]uint64
	seriesLastAt time.Time

	// spMu 串行化状态页列表（维护窗口/人工事件）的读-改-写。
	// settings 的读写各自加锁，但「读列表 → 修改 → 写回」跨了两次加锁，
	// 两个并发管理员操作会互相覆盖。
	spMu sync.Mutex

	notifyJobs   map[string]*model.NotifyJob
	notifyStats  model.NotifyStats
	notifySilent map[string]*model.Silence // scope(global|ruleID) -> 静默期

	traffic    map[string]*model.TrafficCycle
	renewals   []*model.Renewal
	apiTokens  map[string]*model.APIToken // id -> token（只含摘要）
	events     []model.Event
	expirySent map[string]string // nodeID -> 最近一次到期提醒日期

	history       map[string]*nodeHistory
	historyPath   string
	historyDirty  bool
	historySaveMu sync.Mutex

	// 审计日志独立于快照以 JSONL 追加写入：管理操作频率低，
	// 放主快照会让每分钟一次的全量保存携带大量只增不改的数据。
	auditPath   string
	auditMu     sync.Mutex
	audit       []*model.AuditEntry // 按时间升序，只保留最近 auditMemoryLimit 条
	auditWrites int

	path    string
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

	// 分段耗时：优化快照时必须先知道时间花在哪一段。
	// Copy=锁内深拷贝，Marshal=JSON 编码，Encode=gzip 压缩，Write=写盘+rename。
	LastCopySeconds    float64
	LastMarshalSeconds float64
	LastEncodeSeconds  float64
	LastWriteSeconds   float64
	LastBytes          int64

	// 历史分片：每次快照写多少个文件、多少字节、耗时多少。
	// 重构成分片后，主快照体积与耗时大幅下降，这部分成为新的主要成本，
	// 必须单独可观测（否则优化无处验证）。
	LastSeriesSeconds float64
	LastSeriesBytes   int64
	LastSeriesFiles   int
}

// New 创建并尝试从磁盘恢复状态。
//
// 快照损坏时不会静默以空状态启动（那会让全部节点与令牌凭空消失），
// 而是把损坏文件改名隔离，并通过 LoadErr 报告，由调用方决定是否退出。
func New(dataDir string) *Store {
	s := &Store{
		nodes:        map[string]*model.Node{},
		hosts:        map[string]model.HostInfo{},
		series:       map[string]*Ring[model.Metrics]{},
		services:     map[string]*model.Service{},
		svcRes:       map[string]*Ring[model.ServiceSample]{},
		rules:        map[string]*model.AlertRule{},
		settings:     map[string]string{},
		notifyJobs:   map[string]*model.NotifyJob{},
		notifySilent: map[string]*model.Silence{},
		traffic:      map[string]*model.TrafficCycle{},
		apiTokens:    map[string]*model.APIToken{},
		expirySent:   map[string]string{},
		history:      map[string]*nodeHistory{},
		dropped:      map[string]int64{},
		seriesSeq:    map[string]uint64{},
		seriesSaved:  map[string]uint64{},
		path:         filepath.Join(dataDir, "state.json"),
		historyPath:  filepath.Join(dataDir, "history.json"),
		auditPath:    filepath.Join(dataDir, "audit.jsonl"),
		saveReq:      make(chan struct{}, 1),
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		s.loadErr = fmt.Errorf("创建数据目录 %s: %w", dataDir, err)
		return s
	}
	s.loadErr = s.load()
	if s.loadErr == nil {
		s.loadErr = s.loadSeriesShards()
	}
	if s.loadErr == nil {
		s.loadErr = s.loadHistory()
	}
	if s.loadErr == nil {
		// 审计文件损坏不应阻止服务启动：它只是旁路记录，
		// 隔离坏文件后继续提供监控能力。
		s.loadAudit()
	}
	if s.loadErr == nil {
		// 从旧格式（series 内嵌在 state.json）升级上来时，主快照里已有的点
		// 还没有对应的分片文件：把它们标脏，让第一次 Save 完成迁移。
		// 已经读过分片的节点在 loadSeriesShards 里把 seq/saved 都设成 1，
		// 因此不会被重复写一遍。
		s.mu.Lock()
		for id := range s.series {
			if _, ok := s.seriesSeq[id]; !ok {
				s.seriesSeq[id] = 1
			}
		}
		s.mu.Unlock()
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
// v4 增加月度流量周期、续费记录、范围化令牌与活动流；
// 旧快照中缺失的字段均为零值，可无缝载入。
//
// v5 把探测历史从「Metrics 字段复用」改为独立 ServiceSample（ts/lat/ok/cert）。
// 新程序能读 v4（UnmarshalJSON 兼容旧键），但**旧程序读不懂 v5** ——
// 若不提升版本号，降级回 v1.17.x 会把探测历史静默读成"延迟 0、全部失败"，
// 看起来像所有站点同时挂掉。提升后旧程序会明确拒绝并备份，而不是给假数据。
const SnapshotSchemaVersion = 5

type snapshot struct {
	SchemaVersion int                              `json:"schemaVersion"`
	Nodes         map[string]*model.Node           `json:"nodes"`
	Hosts         map[string]model.HostInfo        `json:"hosts"`
	Series        map[string][]model.Metrics       `json:"series"`
	Services      map[string]*model.Service        `json:"services"`
	SvcRes        map[string][]model.ServiceSample `json:"svcRes"`
	Rules         map[string]*model.AlertRule      `json:"rules"`
	Alerts        []*model.Alert                   `json:"alerts"`
	// Tokens 单独持久化：model.Node.Token 标了 json:"-"，
	// 不会随节点一起写入，否则重启后 Agent 全部认证失败。
	Tokens       map[string]string              `json:"tokens"`
	Settings     map[string]string              `json:"settings"`
	NotifyJobs   []*model.NotifyJob             `json:"notifyJobs,omitempty"`
	NotifyStats  model.NotifyStats              `json:"notifyStats"`
	NotifySilent map[string]*model.Silence      `json:"notifySilences,omitempty"`
	Traffic      map[string]*model.TrafficCycle `json:"traffic,omitempty"`
	Renewals     []*model.Renewal               `json:"renewals,omitempty"`
	APITokens    map[string]*model.APIToken     `json:"apiTokens,omitempty"`
	// APITokenHashes 与节点令牌同理：APIToken.Hash 标了 json:"-"，不单独
	// 持久化的话重启后所有自动化令牌都会变成无法匹配的幽灵记录。
	APITokenHashes map[string]string `json:"apiTokenHashes,omitempty"`
	Events         []model.Event     `json:"events,omitempty"`
	ExpirySent     map[string]string `json:"expirySent,omitempty"`
	SavedAt        int64             `json:"savedAt"`
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
		// 服务已不存在的历史直接丢弃：删除服务的那一刻若正好有一次探测
		// 在飞，它的结果会给已删服务重建一个环，此后这份数据既没人看、
		// 也不会自己过期。加载时顺手清掉，免得脏数据一直跟着快照走。
		if _, ok := s.services[k]; !ok {
			continue
		}
		sort.Slice(pts, func(i, j int) bool { return pts[i].TS < pts[j].TS })
		r := NewServiceRing(ServiceRingSize)
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
	for k, v := range snap.Traffic {
		if v != nil {
			s.traffic[k] = v
		}
	}
	for _, r := range snap.Renewals {
		if r != nil && r.ID != "" {
			s.renewals = append(s.renewals, r)
		}
	}
	for k, v := range snap.APITokens {
		if v == nil || v.ID == "" {
			continue
		}
		v.Hash = snap.APITokenHashes[k]
		// 早期 v4 构建没有保存摘要，这类令牌已无法再通过鉴权；
		// 丢弃它们，避免面板里留下永远 401 的幽灵令牌。
		if v.Hash == "" {
			continue
		}
		s.apiTokens[k] = v
	}
	s.events = append(s.events, snap.Events...)
	for k, v := range snap.ExpirySent {
		if k != "" && v != "" {
			s.expirySent[k] = v
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
	var tm saveTiming
	defer func() { s.recordSave(time.Since(start), tm, err) }()

	// ★ saveMu 必须在抓快照之前获取。
	// 若先抓快照、序列化完才拿 saveMu，两个并发 Save 可能
	// 「先抓旧状态、后抓新状态」却按相反顺序落盘，旧快照覆盖新快照：
	// 典型后果是刚轮换的节点令牌被抹掉，重启后 Agent 全部 401。
	// 锁顺序固定为 saveMu → s.mu；Save 的调用方都不持有 s.mu。
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	s.mu.RLock()
	copyStart := time.Now()
	snap := snapshot{
		SchemaVersion: SnapshotSchemaVersion,
		Nodes:         make(map[string]*model.Node, len(s.nodes)),
		Hosts:         make(map[string]model.HostInfo, len(s.hosts)),
		Services:      make(map[string]*model.Service, len(s.services)),
		Rules:         make(map[string]*model.AlertRule, len(s.rules)),
		Alerts:        make([]*model.Alert, 0, len(s.alerts)),
		// ★ Series 刻意留空：历史点走 data/series/<id>.json.gz 分片，
		// 只有读取旧版本快照时才需要这个字段（见 load）。
		SvcRes:         make(map[string][]model.ServiceSample, len(s.svcRes)),
		Tokens:         make(map[string]string, len(s.nodes)),
		Settings:       make(map[string]string, len(s.settings)),
		NotifyJobs:     make([]*model.NotifyJob, 0, len(s.notifyJobs)),
		NotifyStats:    s.notifyStats,
		NotifySilent:   make(map[string]*model.Silence, len(s.notifySilent)),
		Traffic:        make(map[string]*model.TrafficCycle, len(s.traffic)),
		Renewals:       make([]*model.Renewal, 0, len(s.renewals)),
		APITokens:      make(map[string]*model.APIToken, len(s.apiTokens)),
		APITokenHashes: make(map[string]string, len(s.apiTokens)),
		Events:         append([]model.Event(nil), s.events...),
		ExpirySent:     make(map[string]string, len(s.expirySent)),
		SavedAt:        time.Now().Unix(),
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
	for k, tc := range s.traffic {
		cp := *tc
		snap.Traffic[k] = &cp
	}
	for _, r := range s.renewals {
		cp := *r
		snap.Renewals = append(snap.Renewals, &cp)
	}
	for k, t := range s.apiTokens {
		cp := *t
		cp.Hash = "" // 摘要单独存放，避免意外出现在任何令牌 JSON 视图里
		cp.Scopes = append([]string(nil), t.Scopes...)
		cp.Paths = append([]string(nil), t.Paths...)
		snap.APITokens[k] = &cp
		snap.APITokenHashes[k] = t.Hash
	}
	for k, v := range s.expirySent {
		snap.ExpirySent[k] = v
	}
	for k, r := range s.svcRes {
		snap.SvcRes[k] = r.Slice()
	}
	// 历史分片按**独立节奏**写（默认 5 分钟），不跟随每次 Save。
	//
	// 原因：30 秒上报 + 60 秒保存的组合下，每次保存时几乎所有节点都变脏，
	// "增量"起不到作用；而 gzip 压缩 30MB 历史的总成本一分不少（实测
	// 40 台 257ms，比整份快照的 229ms 还多了一点文件开销）。
	// 把分片降到 5 分钟写一次后：60 秒的保存只剩 3KB 主快照（约 1ms），
	// 每 5 分钟才出现一次几十到几百毫秒的尖峰。
	// 代价是崩溃最多丢 5 分钟历史（配置与当前态不丢），对监控完全可接受。
	now := time.Now()
	var shardTasks []shardTask
	if s.seriesLastAt.IsZero() || now.Sub(s.seriesLastAt) >= seriesMinInterval {
		shardTasks = s.collectShardTasks()
	}
	s.mu.RUnlock()
	tm.Copy = time.Since(copyStart)

	// 先写历史分片，再写主快照。
	// 顺序很重要：主快照一旦写成功就代表"这一版状态已落盘"，
	// 若它先于分片落盘而进程崩溃，重启后会读到新配置 + 旧历史（少一段），
	// 反过来则是旧配置 + 新历史（历史多一段），后者危害更小。
	st2, shardErr := s.flushSeriesShards(shardTasks, now.Unix())
	tm.Series, tm.SeriesBytes, tm.SeriesFiles = st2.Series, st2.SeriesBytes, st2.SeriesFiles
	if shardErr != nil {
		// 单个分片失败不阻断主快照：主快照是配置的唯一副本。
		// 失败的分片 saved < seq，下一次 Save 会自动重试。
		log.Printf("保存历史分片部分失败（下一次重试）: %v", shardErr)
	} else if len(shardTasks) > 0 {
		s.seriesLastAt = now
	}

	marshalStart := time.Now()
	raw, err := json.Marshal(&snap)
	if err != nil {
		return err
	}
	tm.Marshal = time.Since(marshalStart)

	encodeStart := time.Now()
	enc, err := encodeSnapshot(raw)
	if err != nil {
		return fmt.Errorf("压缩快照: %w", err)
	}
	tm.Encode = time.Since(encodeStart)

	writeStart := time.Now()
	// 写入已被函数开头的 saveMu 串行化：固定 temp 路径下，
	// 两个并发 Save 会互相截断/覆盖。
	tmp := s.path + ".tmp"
	if werr := os.WriteFile(tmp, enc, 0o600); werr != nil {
		return werr
	}
	err = os.Rename(tmp, s.path)
	tm.Write = time.Since(writeStart)
	tm.Bytes = int64(len(enc))
	if err != nil {
		return err
	}
	// 主快照成功后再清理：万一清理逻辑有 bug，也不会让"配置"先丢。
	if n := s.pruneSeriesShards(); n > 0 {
		log.Printf("清理已删除节点的历史分片 %d 个", n)
	}
	return nil
}

// saveTiming 记录一次快照各阶段的耗时，用于定位瓶颈。
type saveTiming struct {
	Copy        time.Duration // 锁内深拷贝
	Marshal     time.Duration // JSON 编码
	Encode      time.Duration // gzip 压缩到内存
	Write       time.Duration // 写临时文件 + rename
	Bytes       int64
	Series      time.Duration // 写历史分片
	SeriesBytes int64
	SeriesFiles int
}

// recordSave 汇总落盘耗时，供 /metrics 与运维排查使用。
// 失败时只累计次数，保留上一次成功的耗时作为参考。
func (s *Store) recordSave(d time.Duration, tm saveTiming, err error) {
	s.saveStatMu.Lock()
	defer s.saveStatMu.Unlock()
	s.saveStats.Count++
	if err != nil {
		s.saveStats.Failures++
		return
	}
	s.saveStats.LastDurationSeconds = d.Seconds()
	s.saveStats.LastCopySeconds = tm.Copy.Seconds()
	s.saveStats.LastMarshalSeconds = tm.Marshal.Seconds()
	s.saveStats.LastEncodeSeconds = tm.Encode.Seconds()
	s.saveStats.LastWriteSeconds = tm.Write.Seconds()
	s.saveStats.LastBytes = tm.Bytes
	s.saveStats.LastSeriesSeconds = tm.Series.Seconds()
	s.saveStats.LastSeriesBytes = tm.SeriesBytes
	s.saveStats.LastSeriesFiles = tm.SeriesFiles
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

// markDroppedLocked 记录墓碑。容量有上限：大量增删时按"最久未用"清掉旧条目，
// 避免一个只增不减的 map 长期占用内存。
func (s *Store) markDroppedLocked(id string) {
	if s.dropped == nil {
		s.dropped = make(map[string]int64, 8)
	}
	s.dropped[id] = time.Now().Unix()
	const maxDropped = 512
	if len(s.dropped) > maxDropped {
		// 删掉最早的一半：墓碑只需要覆盖"删除瞬间"的在途请求。
		cut := make([]string, 0, len(s.dropped)/2)
		oldest := time.Now().Unix()
		for _, ts := range s.dropped {
			if ts < oldest {
				oldest = ts
			}
		}
		threshold := oldest + int64(maxDropped/2)
		for k, ts := range s.dropped {
			if ts <= threshold {
				cut = append(cut, k)
			}
		}
		if len(cut) == 0 {
			for k := range s.dropped {
				cut = append(cut, k)
				break
			}
		}
		for _, k := range cut {
			delete(s.dropped, k)
		}
	}
}

func (s *Store) UpsertNode(n *model.Node) {
	s.mu.Lock()
	// 重新创建同 ID 节点时撤销墓碑，否则新节点永远收不到上报。
	delete(s.dropped, n.ID)
	if _, ok := s.nodes[n.ID]; !ok {
		if s.series[n.ID] == nil {
			s.series[n.ID] = NewRing(RingSize)
		}
	}
	s.nodes[n.ID] = n
	s.mu.Unlock()
	// 新增节点必须立刻落盘：只靠 60 秒自动快照时，这期间崩溃/重启
	// 会连同它刚生成的上报令牌一起丢失，该 Agent 重启后直接 401。
	s.RequestSave()
}

// UpsertNodes 批量写入节点，整批只落盘一次。
//
// 逐条 UpsertNode 会为每个节点各写一次快照：导入 40 台就是 40 次全量 gzip，
// 而它们本就该是一次操作。语义与 UpsertNode 相同（撤销墓碑、补建序列）。
func (s *Store) UpsertNodes(nodes []*model.Node) {
	if len(nodes) == 0 {
		return
	}
	s.mu.Lock()
	for _, n := range nodes {
		delete(s.dropped, n.ID)
		if _, ok := s.nodes[n.ID]; !ok {
			if s.series[n.ID] == nil {
				s.series[n.ID] = NewRing(RingSize)
			}
		}
		s.nodes[n.ID] = n
	}
	s.mu.Unlock()
	// 与单条一致：新建节点立刻落盘，否则崩溃会连同刚生成的令牌一起丢。
	s.RequestSave()
}

// UpdateNode 在持锁状态下按字段更新单个节点。
//
// 批量操作若先 Node() 取副本、改完再 UpsertNode() 整条写回，会把两次
// 调用之间 Agent 上报写入的 AgentVersion 覆盖回旧值；更严重的是，节点
// 若已被并发删除，整条写回会让它连同旧令牌一起「复活」。
// 返回值表示节点是否存在（已被删除时不做任何写入）。
func (s *Store) UpdateNode(id string, fn func(*model.Node)) bool {
	s.mu.Lock()
	n, ok := s.nodes[id]
	if !ok {
		s.mu.Unlock()
		return false
	}
	fn(n)
	s.mu.Unlock()
	s.RequestSave()
	return true
}

// DeleteNode 删除节点及其监控数据，并关闭其所有未恢复的告警。
// 返回 false 表示节点已经不存在。
//
// 告警必须一并关闭：否则被删节点的活跃告警会永远挂在「进行中」，
// 而且它已不在 Nodes() 里，Evaluate 的恢复分支永远走不到 —— 成为无法自愈的僵尸告警。
func (s *Store) DeleteNode(id string) bool {
	s.mu.Lock()
	if _, ok := s.nodes[id]; !ok {
		s.mu.Unlock()
		return false
	}
	delete(s.nodes, id)
	delete(s.series, id)
	delete(s.hosts, id)
	delete(s.traffic, id)
	delete(s.expirySent, id)
	delete(s.seriesSeq, id)
	delete(s.seriesSaved, id)
	// 墓碑：拦住删除瞬间仍在途的那次上报（见 AddMetrics）。
	s.markDroppedLocked(id)
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
	s.mu.Unlock()
	// 分片文件在锁外删除：盘上 IO 不该占着写锁。
	// 删不掉也不致命：Save 末尾的 pruneSeriesShards 会再兜一次。
	if err := os.Remove(s.shardPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("删除节点 %s 的历史分片失败（稍后清理）: %v", id, err)
	}
	s.RequestSave()
	return true
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
	// ServiceIDs 同样是引用字段：漏掉它会让快照与运行态共享底层数组，
	// 后续 append 扩容时与 json.Marshal 的迭代撞车（与 NodeIDs/Groups 同一类问题）。
	if r.ServiceIDs != nil {
		cp.ServiceIDs = append([]string(nil), r.ServiceIDs...)
	}
	return cp
}

// seriesMinInterval 是历史分片的最小写间隔。
//
// 它决定了"崩溃最多丢多少历史"（配置与当前态从不丢）。
// 调小=历史更及时但写放大；调大=更省 IO 但丢得更多。
const seriesMinInterval = 5 * time.Minute

// FlushSeries 强制把脏的历史分片写盘，忽略最小间隔。
// 调用时机：进程正常退出、以及任何"希望历史立刻完整"的操作。
func (s *Store) FlushSeries() error {
	// 与 Save 共用 saveMu：避免两个分片写并发 rename 同一个文件。
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	s.mu.RLock()
	tasks := s.collectShardTasks()
	s.mu.RUnlock()
	if len(tasks) == 0 {
		return nil
	}
	now := time.Now()
	tm, err := s.flushSeriesShards(tasks, now.Unix())
	if err != nil {
		return err
	}
	s.seriesLastAt = now
	log.Printf("退出前写入历史分片 %d 个（%.1f KB，%.0f ms）",
		tm.SeriesFiles, float64(tm.SeriesBytes)/1024, tm.Series.Seconds()*1000)
	return nil
}

// ---------- 指标 ----------

func (s *Store) AddMetrics(nodeID string, host model.HostInfo, m model.Metrics, nicSet ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 节点刚被删除时丢弃在途上报：删除与上报是并发的，删完之后再写一次
	// 会重建 series/hosts/traffic，表现为"删掉的节点又出现在导出与统计里"，
	// 而且它再也拿不到后续上报，无法自愈。
	// 只拒"明确删除过"的 ID（墓碑），不拒未知 ID —— 历史回填、demo 播种
	// 等路径可以先有数据后建节点。
	if _, dropped := s.dropped[nodeID]; dropped {
		return
	}
	// 统计网卡集合指纹（v1.27.0 起的 Agent 经 X-NIC-Set 头带来）：
	// 集合变化 = 累计计数器口径变化，updateTrafficLocked 据此跳过一次增量
	// 并标记周期不完整。可变参数是为兼容历史回填/demo 播种等无指纹的调用方。
	if len(nicSet) > 0 && nicSet[0] != "" {
		s.handleNICSetLocked(nodeID, nicSet[0])
	}
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
			// ★ 覆盖也算"变了"：不标脏的话这次更新不会进分片，
			// 重启后历史里还是旧值（表现为"最后几个点回退"）。
			s.writeSeq++
			s.seriesSeq[nodeID] = s.writeSeq
			return
		}
	}
	r.Add(m)
	// 标记该节点的历史分片需要重写（Save 只写 seq > saved 的节点）。
	s.writeSeq++
	s.seriesSeq[nodeID] = s.writeSeq
	s.addHistoryLocked(nodeID, m, &prev, hasPrev)
	s.updateTrafficLocked(nodeID, m)
}

// handleNICSetLocked 对比节点上一次上报的网卡集合指纹：
// 变化时置 pendingNICReset，让紧随其后的 updateTrafficLocked 跳过一次增量。
// 必须与 AddMetrics 同锁：对比、置位、消费在同一个临界区里完成，没有崩溃窗口。
// 首次上报（旧值为空）只记录不重置——第一次没有"口径变化"可言。
func (s *Store) handleNICSetLocked(nodeID, fp string) {
	n := s.nodes[nodeID]
	if n == nil {
		return
	}
	old := n.NICSet
	n.NICSet = fp
	if old != "" && old != fp {
		if s.nicResetPending == nil {
			s.nicResetPending = map[string]bool{}
		}
		s.nicResetPending[nodeID] = true
	}
}

// updateTrafficLocked 在指标入库时累加流量周期用量。
// m.TS 已被服务端校正为当前时间，因此重置日按服务端时区计算。
func (s *Store) updateTrafficLocked(nodeID string, m model.Metrics) {
	n := s.nodes[nodeID]
	if n == nil {
		return
	}
	start := trafficCycleStart(time.Unix(m.TS, 0), n.TrafficResetDay)
	tc := s.traffic[nodeID]
	if tc == nil {
		s.traffic[nodeID] = &model.TrafficCycle{
			Start: start, LastRx: m.NetRx, LastTx: m.NetTx,
		}
		return
	}
	// 网卡集合刚发生变化：新读数与旧基线不可比（换了一组网卡求和），
	// 跳变部分绝不能记成真实流量。只重建基线、标记周期不完整，
	// 让"校正本期"有机会把服务商面板上的真实用量补进来。
	if s.nicResetPending[nodeID] {
		delete(s.nicResetPending, nodeID)
		if tc.Start == start {
			tc.LastRx, tc.LastTx = m.NetRx, m.NetTx
			tc.Incomplete = true
			return
		}
		// 恰逢周期切换则随新周期一并重建，不完整标记不带进新周期
		*tc = model.TrafficCycle{Start: start, LastRx: m.NetRx, LastTx: m.NetTx}
		return
	}
	if tc.Start != start {
		// 真正的新周期：用量清零，从当前计数重新起算。
		*tc = model.TrafficCycle{Start: start, LastRx: m.NetRx, LastTx: m.NetTx}
		return
	}
	if m.TS < tc.Start || (tc.LastRx > m.NetRx && tc.LastTx > m.NetTx) {
		// 同一周期内的异常：时钟回拨，或计数器归零（Agent 重启 / 网卡重载）。
		// ★ 必须保留本周期已累计的用量，只重置基线 ——
		// 原来这里整体重建结构体，等于把本周期已经跑掉的流量抹掉。
		tc.LastRx, tc.LastTx = m.NetRx, m.NetTx
		return
	}
	if m.NetRx >= tc.LastRx {
		tc.UsedRx += m.NetRx - tc.LastRx
	} else {
		// 只有下行计数器归零时不能丢掉上行增量：分别判断。
		tc.UsedRx += m.NetRx
	}
	if m.NetTx >= tc.LastTx {
		tc.UsedTx += m.NetTx - tc.LastTx
	} else {
		tc.UsedTx += m.NetTx
	}
	tc.LastRx, tc.LastTx = m.NetRx, m.NetTx
}

// trafficCycleStart 返回 ts 所在流量周期的起始时刻。
// ResetDay 限定 1–28，避免 2 月没有 30 日导致周期漂移。
func trafficCycleStart(ts time.Time, resetDay int) int64 {
	if resetDay < 1 || resetDay > 28 {
		resetDay = 1
	}
	t := ts.In(time.Local)
	start := time.Date(t.Year(), t.Month(), resetDay, 0, 0, 0, 0, time.Local)
	if t.Before(start) {
		start = start.AddDate(0, -1, 0)
	}
	return start.Unix()
}

// TrafficUsage 返回节点当前周期的流量用量。
// Used 已按节点配置的计费口径合成，调用方不要再自行把 UsedRx+UsedTx 相加 ——
// 单边计费或取大者口径下那样算会得出偏大的用量。
func (s *Store) TrafficUsage(nodeID string) model.TrafficUsage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := model.TrafficUsage{Mode: model.TrafficModeSum}
	if n := s.nodes[nodeID]; n != nil {
		out.QuotaGB = n.TrafficQuotaGB
		out.ResetDay = n.TrafficResetDay
		out.Mode = model.NormalizeTrafficMode(n.TrafficMode)
	}
	if tc := s.traffic[nodeID]; tc != nil {
		out.UsedRx, out.UsedTx = tc.UsedRx, tc.UsedTx
		out.CycleStart = tc.Start
		out.Incomplete = tc.Incomplete
	}
	out.Used = model.TrafficUsed(out.Mode, out.UsedRx, out.UsedTx)
	return out
}

// ---------- 活动流 ----------

const maxEvents = 200

// AddEvent 追加一条公开活动流事件。
func (s *Store) AddEvent(e model.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addEventLocked(e)
}

func (s *Store) addEventLocked(e model.Event) {
	if e.Text == "" {
		return
	}
	if e.TS <= 0 {
		e.TS = time.Now().Unix()
	}
	if e.ID == "" {
		e.ID = NewID("ev")
	}
	s.events = append(s.events, e)
	if len(s.events) > maxEvents {
		s.events = append([]model.Event(nil), s.events[len(s.events)-maxEvents:]...)
	}
}

// Events 返回最近的事件（新的在前）。
func (s *Store) Events(limit int) []model.Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Event, 0, len(s.events))
	for i := len(s.events) - 1; i >= 0; i-- {
		out = append(out, s.events[i])
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// ---------- 续费记录 ----------

func (s *Store) Renewals(nodeID string) []*model.Renewal {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*model.Renewal, 0, len(s.renewals))
	for _, r := range s.renewals {
		if nodeID != "" && r.NodeID != nodeID {
			continue
		}
		cp := *r
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At > out[j].At })
	return out
}

func (s *Store) AddRenewal(r *model.Renewal) {
	s.mu.Lock()
	cp := *r
	s.renewals = append(s.renewals, &cp)
	s.mu.Unlock()
	s.RequestSave()
}

func (s *Store) DeleteRenewal(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, r := range s.renewals {
		if r.ID == id {
			s.renewals = append(s.renewals[:i], s.renewals[i+1:]...)
			s.RequestSave()
			return true
		}
	}
	return false
}

// ExpirySentFor 返回今天是否已为该节点发过到期提醒。
func (s *Store) ExpirySentFor(nodeID, day string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expirySent[nodeID] == day
}

// MarkExpirySent 原子地标记「今天已为这个节点发过到期提醒」。
// 返回 false 表示今天已经发过，调用方应跳过。
func (s *Store) MarkExpirySent(nodeID, day string) bool {
	s.mu.Lock()
	if s.expirySent[nodeID] == day {
		s.mu.Unlock()
		return false
	}
	s.expirySent[nodeID] = day
	s.mu.Unlock()
	s.RequestSave()
	return true
}

// ---------- 范围化 API 令牌 ----------

// AddAPIToken 保存令牌摘要。明文由调用方生成并只返回一次。
func (s *Store) AddAPIToken(t *model.APIToken) {
	s.mu.Lock()
	cp := *t
	cp.Scopes = append([]string(nil), t.Scopes...)
	cp.Paths = append([]string(nil), t.Paths...)
	s.apiTokens[cp.ID] = &cp
	s.mu.Unlock()
	s.RequestSave()
}

// APITokens 返回不含摘要的令牌列表。
func (s *Store) APITokens() []*model.APIToken {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*model.APIToken, 0, len(s.apiTokens))
	for _, t := range s.apiTokens {
		cp := *t
		cp.Hash = ""
		cp.Scopes = append([]string(nil), t.Scopes...)
		cp.Paths = append([]string(nil), t.Paths...)
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

// DeleteAPIToken 删除令牌。
func (s *Store) DeleteAPIToken(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.apiTokens[id]; !ok {
		return false
	}
	delete(s.apiTokens, id)
	s.RequestSave()
	return true
}

// MatchAPIToken 按摘要查找令牌并更新最近使用时间。
func (s *Store) MatchAPIToken(hash string, now int64) (*model.APIToken, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.apiTokens {
		if subtle.ConstantTimeCompare([]byte(t.Hash), []byte(hash)) != 1 {
			continue
		}
		if t.ExpiresAt > 0 && now >= t.ExpiresAt {
			return nil, false
		}
		// 最近使用时间每分钟最多落盘一次，避免高频抓取导致持续快照。
		// 先更新时间再复制，确保调用方拿到的也是本次使用后的状态。
		if now-t.LastUsedAt >= 60 {
			t.LastUsedAt = now
			s.RequestSave()
		}
		cp := *t
		cp.Scopes = append([]string(nil), t.Scopes...)
		cp.Paths = append([]string(nil), t.Paths...)
		return &cp, true
	}
	return nil, false
}

// SetAgentVersion 记录 Agent 最近一次上报的版本。
// 版本变化才落盘：每次上报都 RequestSave 会让快照队列持续被唤醒。
func (s *Store) SetAgentVersion(nodeID, ver string) {
	s.mu.Lock()
	n, ok := s.nodes[nodeID]
	if !ok || n.AgentVersion == ver {
		s.mu.Unlock()
		return
	}
	n.AgentVersion = ver
	s.mu.Unlock()
	s.RequestSave()
}

// SetNodeIP 记录最近一次通过鉴权上报的来源 IP。
// 与 SetAgentVersion 同样只在变化时落盘：13 台节点 30 秒一报，
// 每次保存都唤醒快照队列毫无意义，而 IP 变动本身极少。
func (s *Store) SetNodeIP(nodeID, ip string) (prev string, changed bool) {
	if ip == "" {
		return "", false
	}
	s.mu.Lock()
	n, ok := s.nodes[nodeID]
	if !ok {
		s.mu.Unlock()
		return "", false
	}
	if n.LastIP == ip {
		s.mu.Unlock()
		return ip, false
	}
	prev = n.LastIP
	n.LastIP = ip
	s.mu.Unlock()
	s.RequestSave()
	return prev, true
}

// SetReportAuth 记录最近一次上报的签名状态与时钟偏差。
//
// 只做"变了才落盘"：时钟偏差逐次抖动 ±1 秒是常态，每报一次都 RequestSave
// 等于用 13 台机器的正常心跳去反复唤醒快照队列。因此偏差要累计到 5 秒
// 才写一次 —— 这个精度对"要不要去校时"的判断完全够用。
func (s *Store) SetReportAuth(nodeID string, signed bool, skew int64) {
	s.mu.Lock()
	n, ok := s.nodes[nodeID]
	if !ok {
		s.mu.Unlock()
		return
	}
	signedChanged := n.Signed != signed
	skewChanged := n.ClockSkew-skew > 5 || skew-n.ClockSkew > 5
	if !signedChanged && !skewChanged {
		s.mu.Unlock()
		return
	}
	n.Signed = signed
	if signed {
		n.ClockSkew = skew
	} else {
		// 未签名时那个时间戳不可信，不能留在台账里误导人。
		n.ClockSkew = 0
	}
	s.mu.Unlock()
	s.RequestSave()
}

// SetNodeLastLogin 记录最近一次交互式登录，返回是否有变化（用于决定要不要通知）。
//
// 只有"更新"才落盘：一条登录事件被上报一次就够，重复上报（Agent 重启后
// 水位重建的边界情况）不该再唤醒快照队列，也不该重复通知。
func (s *Store) SetNodeLastLogin(nodeID string, ev model.LoginEvent) bool {
	s.mu.Lock()
	n, ok := s.nodes[nodeID]
	if !ok {
		s.mu.Unlock()
		return false
	}
	if n.LastLogin != nil && n.LastLogin.TS >= ev.TS {
		// 不比现有更新：既包括重复上报，也包括乱序到达的旧事件。
		s.mu.Unlock()
		return false
	}
	cp := ev
	n.LastLogin = &cp
	s.mu.Unlock()
	s.RequestSave()
	return true
}

// SetNodeLoginState 记录 Agent 自报的登录采集可用性（只在此变化时落盘）。
func (s *Store) SetNodeLoginState(nodeID, state string, age, lastTS int64) {
	s.mu.Lock()
	n, ok := s.nodes[nodeID]
	if !ok {
		s.mu.Unlock()
		return
	}
	// age 逐次抖动，超过 1 小时才更新一次，避免每次心跳都唤醒快照。
	ageChanged := n.LoginAge-age > 3600 || age-n.LoginAge > 3600
	if n.LoginState == state && !ageChanged && n.LoginLastTS == lastTS {
		s.mu.Unlock()
		return
	}
	n.LoginState = state
	// 读不到 wtmp 时 age 是 -1，保留它以便界面区分"不可用"与"刚写过"。
	n.LoginAge = age
	n.LoginLastTS = lastTS
	s.mu.Unlock()
	s.RequestSave()
}

// AdjustTrafficUsed 把节点当前周期的用量直接改成 want（按当前计费口径）。
//
// 用途是"和服务商面板对账"：月中才配好重置日时，本周期已经跑掉的量拿不到，
// 只能人工补录一次。实现上把 want 折算进 UsedRx/UsedTx，随后 Agent 的增量
// 继续在它之上累加 —— 因此必须保留 LastRx/LastTx 作为增量基线，不能一起清掉。
func (s *Store) AdjustTrafficUsed(nodeID string, want uint64) (model.TrafficUsage, bool) {
	s.mu.Lock()
	n, ok := s.nodes[nodeID]
	if !ok {
		s.mu.Unlock()
		return model.TrafficUsage{}, false
	}
	tc := s.traffic[nodeID]
	if tc == nil {
		// 还没有本周期记录：先建一个空周期，基线留 0，下一次上报会补上。
		tc = &model.TrafficCycle{Start: trafficCycleStart(time.Now(), n.TrafficResetDay)}
		s.traffic[nodeID] = tc
	}
	// 按口径把 want 落到收发两个计数器上，保证 TrafficUsage.Used 恰好等于 want。
	switch model.NormalizeTrafficMode(n.TrafficMode) {
	case model.TrafficModeRx:
		tc.UsedRx, tc.UsedTx = want, 0
	case model.TrafficModeTx:
		tc.UsedRx, tc.UsedTx = 0, want
	case model.TrafficModeMax:
		tc.UsedRx, tc.UsedTx = want, want
	default: // sum
		tc.UsedRx, tc.UsedTx = want, 0
	}
	// 人工校正 = 以服务商面板为准，"不完整"的疑虑就此消除。
	tc.Incomplete = false
	s.mu.Unlock()
	s.RequestSave()
	return s.TrafficUsage(nodeID), true
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
	if s.svcRes[sv.ID] == nil {
		s.svcRes[sv.ID] = NewServiceRing(ServiceRingSize)
	}
	s.services[sv.ID] = sv
	s.mu.Unlock()
	s.RequestSave()
}

// CreateService 仅在 ID 不存在时创建服务，返回是否创建成功。
// 管理端 POST 用它避免覆盖同 ID 的已有服务（与节点创建保持一致）。
func (s *Store) CreateService(sv *model.Service) bool {
	s.mu.Lock()
	if _, exists := s.services[sv.ID]; exists {
		s.mu.Unlock()
		return false
	}
	if s.svcRes[sv.ID] == nil {
		s.svcRes[sv.ID] = NewServiceRing(ServiceRingSize)
	}
	cp := *sv
	s.services[sv.ID] = &cp
	s.mu.Unlock()
	s.RequestSave()
	return true
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

// Service 返回单个服务探测目标的副本。
func (s *Store) Service(id string) (*model.Service, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.services[id]
	if !ok {
		return nil, false
	}
	cp := *v
	return &cp, true
}

// AddServiceResult 记录探测结果（独立于指标的轻量环形缓冲）。
func (s *Store) AddServiceResult(sr model.ServiceResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 丢弃"已删除服务"的迟到结果：探针在删除瞬间可能还在飞。
	// 不丢的话这里会给它重建一个环，产生永不显示、永不过期的孤儿数据。
	if _, ok := s.services[sr.ServiceID]; !ok {
		return
	}
	r := s.svcRes[sr.ServiceID]
	if r == nil {
		r = NewServiceRing(ServiceRingSize)
		s.svcRes[sr.ServiceID] = r
	}
	r.Add(model.ServiceSample{TS: sr.TS, LatencyMs: sr.LatencyMs, OK: sr.OK, CertDays: sr.CertDays})
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
		out = append(out, [3]int64{p.TS, int64(p.LatencyMs), int64(b2i(p.OK))})
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
	return p.TS, p.LatencyMs, p.OK, p.CertDays, true
}

// ServiceStats 是对最近一段时间探测结果的抖动/丢包统计。
// 丢包率按失败点数占比计算；抖动按相邻成功探测延迟差的绝对值均值计算。
type ServiceStats struct {
	LossPct  float64 `json:"lossPct"`
	JitterMs float64 `json:"jitterMs"`
	Samples  int     `json:"samples"`
	Has      bool    `json:"has"`
}

func (s *Store) ServiceStats(id string, since int64) ServiceStats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r := s.svcRes[id]
	if r == nil {
		return ServiceStats{}
	}
	pts := r.Since(since)
	if len(pts) == 0 {
		return ServiceStats{}
	}
	var fail, okCount int
	var prevLat float64
	var hasPrev bool
	var jitterSum float64
	for _, p := range pts {
		if !p.OK {
			fail++
			continue
		}
		okCount++
		if hasPrev {
			d := float64(p.LatencyMs) - prevLat
			if d < 0 {
				d = -d
			}
			jitterSum += d
		}
		prevLat, hasPrev = float64(p.LatencyMs), true
	}
	st := ServiceStats{LossPct: float64(fail) / float64(len(pts)) * 100, Samples: len(pts), Has: true}
	if okCount > 1 {
		st.JitterMs = jitterSum / float64(okCount-1)
	}
	return st
}

// ---------- 告警 ----------

func (s *Store) UpsertRule(r *model.AlertRule) {
	s.mu.Lock()
	s.rules[r.ID] = r
	s.mu.Unlock()
	// 与删除路径保持同一持久性：新增/修改的规则也必须尽快落盘。
	s.RequestSave()
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

// ActiveAlertsByService 返回某个服务名下未恢复告警的副本。
// 删除服务前先取快照，删除后再补发恢复通知。
func (s *Store) ActiveAlertsByService(serviceID string) []*model.Alert {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*model.Alert, 0)
	for _, a := range s.alerts {
		if a.ServiceID == serviceID && a.ResolvedAt == 0 {
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
	if _, exists := s.nodes[a.NodeID]; !exists {
		return false, false
	}
	if _, exists := s.rules[a.RuleID]; !exists {
		return false, false
	}
	if a.ServiceID != "" {
		sv, exists := s.services[a.ServiceID]
		if !exists || sv.NodeID != a.NodeID {
			return false, false
		}
	}
	for _, ex := range s.alerts {
		if ex.RuleID == a.RuleID && ex.NodeID == a.NodeID &&
			ex.ServiceID == a.ServiceID && ex.ResolvedAt == 0 {
			// 回填持久化告警的真实 ID，通知任务据此关联告警记录。
			a.ID = ex.ID
			// 只在值/文案真的变化时请求落盘：引擎每 15 秒评估一次，
			// 无条件的 RequestSave 会让活动告警期间永久保持全量快照写入。
			if ex.Value != a.Value || ex.Message != a.Message {
				ex.Value = a.Value
				ex.Message = a.Message
				s.RequestSave()
			}
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
	return s.resolveAlert(ruleID, nodeID, "")
}

// ResolveServiceAlert 关闭服务类告警（唯一键包含 ServiceID）。
func (s *Store) ResolveServiceAlert(ruleID, serviceID string) bool {
	return s.resolveAlert(ruleID, "", serviceID)
}

func (s *Store) resolveAlert(ruleID, nodeID, serviceID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	resolved := false
	for _, a := range s.alerts {
		if a.RuleID != ruleID || a.ServiceID != serviceID || a.ResolvedAt != 0 {
			continue
		}
		// nodeID 为空表示只按服务 ID 匹配；否则必须同时匹配节点。
		if nodeID != "" && a.NodeID != nodeID {
			continue
		}
		a.ResolvedAt = now
		resolved = true
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
	now := time.Now().Unix()
	// 顺带收集本次被关闭的告警 ID：NotifyJob 没有 ServiceID 字段，
	// 只能通过 AlertID 反查它属于哪个服务。
	closedAlerts := make(map[string]bool, 4)
	for _, a := range s.alerts {
		if a.ServiceID == id && a.ResolvedAt == 0 {
			a.ResolvedAt = now
			closedAlerts[a.ID] = true
		}
	}
	// 与 DeleteNode 一致：把已排队但未投递的通知标记为抑制。
	// 否则删掉服务后还会收到它的告警，或留下永不过期的 pending 记录。
	for _, j := range s.notifyJobs {
		if j.Status == model.NotifyPending && closedAlerts[j.AlertID] {
			j.Status = model.NotifySuppressed
			j.SuppressedBy = "service:" + id
		}
	}
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

// SettingKeys 返回指定前缀的设置键（副本）。
// 供运行期标记（如报告去重键）做定期清理，避免状态文件无界增长。
func (s *Store) SettingKeys(prefix string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, 8)
	for k := range s.settings {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out
}

// DeleteSetting 删除一个设置键，返回是否确实删除。
// 与 SetSetting 一样主动请求落盘，避免清理结果在下一次重启后"复活"。
func (s *Store) DeleteSetting(key string) bool {
	s.mu.Lock()
	_, ok := s.settings[key]
	if ok {
		delete(s.settings, key)
	}
	s.mu.Unlock()
	if ok {
		s.RequestSave()
	}
	return ok
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
	// 规则停用后，队列里尚未投递的 firing 通知同样应该作废；
	// 否则恢复通知已经发出，旧告警却还会晚一步到达。
	suppressed := 0
	for _, j := range s.notifyJobs {
		if j.RuleID == ruleID && j.Status == model.NotifyPending {
			j.Status = model.NotifySuppressed
			j.SuppressedBy = ruleID
			j.LastError = "规则已停用"
			j.NextAttemptAt = 0
			s.notifyStats.Suppressed++
			suppressed++
		}
	}
	if suppressed > 0 {
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
	j.FinishedAt = now
	j.LastError = lastError
	j.NextAttemptAt = 0
	s.notifyStats.Sent++
	s.notifyStats.LastSuccess = now
	s.mu.Unlock()
	s.RequestSave()
	return true
}

// NotifyJobRetry 标记本轮失败并安排下一次重试。
// 这里不接收时间戳：任务仍是 pending，没有「完成时间」可记；
// 之前多传的 now 从未被使用，只会让人误以为写了什么。
func (s *Store) NotifyJobRetry(id, lastError string, nextAt int64) bool {
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

// NotifyJobDefer 把待投递任务推迟到 nextAt，且不计入尝试次数。
// 用于免打扰窗口：在窗口内反复“重试”会很快耗尽 MaxAttempts，
// 导致告警在窗口结束后被直接丢弃。
func (s *Store) NotifyJobDefer(id string, nextAt int64) bool {
	s.mu.Lock()
	j, ok := s.notifyJobs[id]
	if !ok || j.Status != model.NotifyPending {
		s.mu.Unlock()
		return false
	}
	j.NextAttemptAt = nextAt
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
	j.FinishedAt = now
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
	j.FinishedAt = now
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
	// 重新入队后它不再是终态任务：保留 FinishedAt/SentAt 会让面板把
	// 「投递中」的任务显示成早已失败/成功的时间。
	j.FinishedAt = 0
	j.SentAt = 0
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
	changed := s.pruneNotifyLocked(now)
	s.mu.Unlock()
	if changed {
		s.RequestSave()
	}
}

// pruneNotifyLocked 返回是否有条目被删除，调用方据此决定是否落盘。
func (s *Store) pruneNotifyLocked(now int64) bool {
	changed := false
	retain := int64(NotifyJobRetention / time.Second)
	for id, j := range s.notifyJobs {
		if j.Status == model.NotifyPending {
			continue
		}
		// 失败/静默任务没有 SentAt，FinishedAt 才是它们进入终态的时间。
		ts := j.CreatedAt
		if j.SentAt > ts {
			ts = j.SentAt
		}
		if j.FinishedAt > ts {
			ts = j.FinishedAt
		}
		if ts > 0 && now-ts > retain {
			delete(s.notifyJobs, id)
			changed = true
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
			changed = true
		}
	}
	for scope, sl := range s.notifySilent {
		if sl.Until > 0 && sl.Until <= now {
			delete(s.notifySilent, scope)
			changed = true
		}
	}
	return changed
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

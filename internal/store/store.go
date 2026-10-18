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
	path     string
	dirty    bool
	loadErr  error
	saveMu   sync.Mutex
}

// New 创建并尝试从磁盘恢复状态。
//
// 快照损坏时不会静默以空状态启动（那会让全部节点与令牌凭空消失），
// 而是把损坏文件改名隔离，并通过 LoadErr 报告，由调用方决定是否退出。
func New(dataDir string) *Store {
	s := &Store{
		nodes:    map[string]*model.Node{},
		hosts:    map[string]model.HostInfo{},
		series:   map[string]*Ring{},
		services: map[string]*model.Service{},
		svcRes:   map[string]*Ring{},
		rules:    map[string]*model.AlertRule{},
		settings: map[string]string{},
		path:     filepath.Join(dataDir, "state.json"),
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		s.loadErr = fmt.Errorf("创建数据目录 %s: %w", dataDir, err)
		return s
	}
	s.loadErr = s.load()
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

type snapshot struct {
	Nodes    map[string]*model.Node      `json:"nodes"`
	Hosts    map[string]model.HostInfo   `json:"hosts"`
	Series   map[string][]model.Metrics  `json:"series"`
	Services map[string]*model.Service   `json:"services"`
	SvcRes   map[string][]model.Metrics  `json:"svcRes"`
	Rules    map[string]*model.AlertRule `json:"rules"`
	Alerts   []*model.Alert              `json:"alerts"`
	// Tokens 单独持久化：model.Node.Token 标了 json:"-"，
	// 不会随节点一起写入，否则重启后 Agent 全部认证失败。
	Tokens   map[string]string `json:"tokens"`
	Settings map[string]string `json:"settings"`
	SavedAt  int64             `json:"savedAt"`
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
			return fmt.Errorf("快照损坏且无法隔离 %s: %w", s.path, err)
		}
		return fmt.Errorf("快照损坏，已备份到 %s: %w", bak, err)
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
func (s *Store) Save() error {
	s.mu.RLock()
	snap := snapshot{
		Nodes:    make(map[string]*model.Node, len(s.nodes)),
		Hosts:    make(map[string]model.HostInfo, len(s.hosts)),
		Services: make(map[string]*model.Service, len(s.services)),
		Rules:    make(map[string]*model.AlertRule, len(s.rules)),
		Alerts:   make([]*model.Alert, 0, len(s.alerts)),
		Series:   make(map[string][]model.Metrics, len(s.series)),
		SvcRes:   make(map[string][]model.Metrics, len(s.svcRes)),
		Tokens:   make(map[string]string, len(s.nodes)),
		Settings: make(map[string]string, len(s.settings)),
		SavedAt:  time.Now().Unix(),
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
		cp := *v
		snap.Rules[k] = &cp
	}
	for _, a := range s.alerts {
		cp := *a
		snap.Alerts = append(snap.Alerts, &cp)
	}
	for k, v := range s.settings {
		snap.Settings[k] = v
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
	// 串行化磁盘写入：固定 temp 路径下，两个并发 Save 会互相截断/覆盖。
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// StartAutoSave 启动后台快照。
func (s *Store) StartAutoSave(every time.Duration) {
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for range t.C {
			if err := s.Save(); err != nil {
				log.Printf("自动保存失败: %v", err)
			}
		}
	}()
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
	now := time.Now().Unix()
	for _, a := range s.alerts {
		if a.NodeID == id && a.ResolvedAt == 0 {
			a.ResolvedAt = now
		}
	}
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

// ---------- 指标 ----------

func (s *Store) AddMetrics(nodeID string, host model.HostInfo, m model.Metrics) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.series[nodeID]
	if r == nil {
		r = NewRing(RingSize)
		s.series[nodeID] = r
	}
	r.Add(m)
	if host.Hostname != "" {
		s.hosts[nodeID] = host
	}
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
		cp := *v
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
	defer s.mu.Unlock()
	if _, ok := s.services[id]; !ok {
		return false
	}
	delete(s.services, id)
	delete(s.svcRes, id)
	return true
}

// ---------- 键值设置 ----------

// SetSetting 保存一段配置（JSON 字符串）。
func (s *Store) SetSetting(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settings == nil {
		s.settings = map[string]string{}
	}
	s.settings[key] = value
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
	now := time.Now().Unix()
	for _, a := range s.alerts {
		if a.RuleID == id && a.ResolvedAt == 0 {
			a.ResolvedAt = now
		}
	}
	return true
}

// ResolveRuleAlerts 关闭某规则名下所有未恢复的告警，返回关闭条数。
// 规则被停用或删除时调用，避免遗留永远无法自愈的「进行中」告警：
// 引擎只对启用中的规则跑恢复分支，停用规则的告警再没有别的关闭路径。
func (s *Store) ResolveRuleAlerts(ruleID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	n := 0
	for _, a := range s.alerts {
		if a.RuleID == ruleID && a.ResolvedAt == 0 {
			a.ResolvedAt = now
			n++
		}
	}
	return n
}

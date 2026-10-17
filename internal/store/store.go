// Package store 实现内存时序存储与 JSON 持久化。
//
// 设计取舍：不引入数据库依赖（对齐 Komari 的"零数据库"理念）。
// 40 台规模、30 秒粒度、保留 24 小时 = 每节点 2880 点，
// 全量常驻内存约 30MB，完全可控；定期快照到磁盘保证重启不丢历史。
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/motao/probe/internal/model"
)

// RingSize 是每节点保留的采样点数。30s × 2880 = 24h。
const RingSize = 2880

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
	// 同一秒内的重复上报覆盖，避免 Agent 重启产生重复点
	if r.n > 0 {
		last := (r.head - 1 + r.size) % r.size
		if r.buf[last].TS == m.TS {
			r.buf[last] = m
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
}

// New 创建并尝试从磁盘恢复状态。
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
	_ = os.MkdirAll(dataDir, 0o750)
	s.load()
	return s
}

// ---------- 快照结构 ----------

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

func (s *Store) load() {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var snap snapshot
	if json.Unmarshal(b, &snap) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range snap.Nodes {
		if tok, ok := snap.Tokens[k]; ok {
			v.Token = tok // 还原上报令牌
		}
		s.nodes[k] = v
	}
	for k, v := range snap.Hosts {
		s.hosts[k] = v
	}
	for k, pts := range snap.Series {
		r := NewRing(RingSize)
		for _, p := range pts {
			r.Add(p)
		}
		s.series[k] = r
	}
	for k, v := range snap.Services {
		s.services[k] = v
	}
	for k, pts := range snap.SvcRes {
		r := NewRing(720)
		for _, p := range pts {
			r.Add(p)
		}
		s.svcRes[k] = r
	}
	for k, v := range snap.Rules {
		s.rules[k] = v
	}
	s.alerts = snap.Alerts
	if snap.Settings != nil {
		s.settings = snap.Settings
	}
}

// Save 将内存状态写入磁盘。
func (s *Store) Save() error {
	s.mu.RLock()
	snap := snapshot{
		Nodes:    s.nodes,
		Hosts:    s.hosts,
		Services: s.services,
		Rules:    s.rules,
		Alerts:   s.alerts,
		Series:   map[string][]model.Metrics{},
		SvcRes:   map[string][]model.Metrics{},
		Tokens:   map[string]string{},
		Settings: s.settings,
		SavedAt:  time.Now().Unix(),
	}
	for k, n := range s.nodes {
		if n.Token != "" {
			snap.Tokens[k] = n.Token
		}
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
			_ = s.Save()
		}
	}()
}

// ---------- 节点 ----------

func NewID(prefix string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

// NewToken 生成节点上报令牌（24 字节随机，hex 编码）。
func NewToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
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

func (s *Store) DeleteNode(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.nodes, id)
	delete(s.series, id)
	delete(s.hosts, id)
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
	if !ok || n.Token != token || token == "" {
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
		s.svcRes[sv.ID] = NewRing(720)
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
		r = NewRing(720)
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

// OpenAlert 断言/维持一条告警；返回是否为新建。
func (s *Store) OpenAlert(a *model.Alert) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ex := range s.alerts {
		if ex.RuleID == a.RuleID && ex.NodeID == a.NodeID && ex.ResolvedAt == 0 {
			ex.Value = a.Value
			ex.Message = a.Message
			return false
		}
	}
	s.alerts = append(s.alerts, a)
	if len(s.alerts) > 500 {
		s.alerts = s.alerts[len(s.alerts)-500:]
	}
	return true
}

// ResolveAlert 关闭匹配的未恢复告警。
func (s *Store) ResolveAlert(ruleID, nodeID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	for _, a := range s.alerts {
		if a.RuleID == ruleID && a.NodeID == nodeID && a.ResolvedAt == 0 {
			a.ResolvedAt = now
		}
	}
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

// PruneSeries 清理超过保留窗口的点（环形缓冲本身会覆盖，此处为一致性保留）。
func (s *Store) PruneSeries() {}

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

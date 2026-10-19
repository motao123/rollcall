package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/motao/rollcall/internal/model"
	"github.com/motao/rollcall/internal/sanitize"
)

// 配置持久化键。内容都是 JSON 字符串，存在 settings map 里，
// 随主快照一起落盘，不额外引入文件或 schema 版本。
const (
	keyStatusPage  = "status_page"
	keyMaintenance = "maintenance"
	keyIncidents   = "incidents"
)

// StatusPage 返回公开状态页配置。
//
// 先铺默认值再反序列化：JSON 中不存在的键保留默认值，
// 这样将来新增字段不会让已保存的老配置被读成 false。
func (s *Store) StatusPage() model.StatusPage {
	cfg := model.DefaultStatusPage()
	raw := s.GetSetting(keyStatusPage)
	if raw == "" {
		return cfg
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		// 配置损坏时返回默认值而不是零值：零值会把所有区块关掉，
		// 公开页变空白，管理员还以为是前端坏了。
		return model.DefaultStatusPage()
	}
	cfg.Normalize()
	return cfg
}

// SetStatusPage 保存公开状态页配置。
// 文本字段统一过 sanitize：这些字符串会直接渲染到公开页，
// 控制符与双向控制符会让"看到的内容"和"实际内容"不一致。
func (s *Store) SetStatusPage(c model.StatusPage) {
	c.Normalize()
	c.Title = sanitize.Text(c.Title, 64)
	c.Announcement.Text = sanitize.Text(c.Announcement.Text, 1000)
	if c.Announcement.Text == "" {
		c.Announcement.Enabled = false
	}
	b, err := json.Marshal(&c)
	if err != nil {
		return
	}
	s.SetSetting(keyStatusPage, string(b))
}

// ---------- 维护窗口 ----------

// Maintenances 返回全部维护窗口，按开始时间升序。
func (s *Store) Maintenances() []*model.Maintenance {
	s.spMu.Lock()
	defer s.spMu.Unlock()
	out, _ := s.maintenancesLocked()
	return out
}

// maintenancesLocked 在持有 spMu 时读取并清洗列表。
// 返回错误而不是空列表：调用方据此拒绝写入，避免把损坏的旧数据覆盖掉。
func (s *Store) maintenancesLocked() ([]*model.Maintenance, error) {
	var out []*model.Maintenance
	raw := s.GetSetting(keyMaintenance)
	if raw == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("维护窗口配置损坏: %w", err)
	}
	// 丢弃结构不完整或时间非法的条目，避免一条坏数据让整个列表不可用
	kept := out[:0]
	for _, m := range out {
		if m == nil || m.ID == "" || m.EndAt <= m.StartAt {
			continue
		}
		kept = append(kept, m)
	}
	out = kept
	sort.Slice(out, func(i, j int) bool { return out[i].StartAt < out[j].StartAt })
	return out, nil
}

// UpsertMaintenance 新增或更新一个维护窗口。
// Title 为空时用一个稳定的占位标题：公开页要显示"为什么在维护"，
// 空标题会让用户只看到一个无解释的徽章。
func (s *Store) UpsertMaintenance(m *model.Maintenance) error {
	if m == nil {
		return errors.New("维护窗口为空")
	}
	if strings.TrimSpace(m.ID) == "" {
		return errors.New("维护窗口缺少 ID")
	}
	m.Title = sanitize.Text(m.Title, 128)
	m.Reason = sanitize.Text(m.Reason, 500)
	if m.Title == "" {
		m.Title = "计划维护"
	}
	if m.EndAt <= m.StartAt {
		return errors.New("维护窗口结束时间必须晚于开始时间")
	}
	s.spMu.Lock()
	defer s.spMu.Unlock()
	list, err := s.maintenancesLocked()
	if err != nil {
		return err
	}
	replaced := false
	for i, cur := range list {
		if cur.ID == m.ID {
			// CreatedAt 是服务端记账字段：更新既有窗口时不能被客户端的
			// now 覆盖，否则「创建时间」会随每次编辑漂移。
			if cur.CreatedAt > 0 {
				m.CreatedAt = cur.CreatedAt
			}
			list[i] = m
			replaced = true
			break
		}
	}
	if !replaced {
		list = append(list, m)
	}
	return s.saveMaintenances(list)
}

// DeleteMaintenance 删除维护窗口。
func (s *Store) DeleteMaintenance(id string) (bool, error) {
	s.spMu.Lock()
	defer s.spMu.Unlock()
	list, err := s.maintenancesLocked()
	if err != nil {
		return false, err
	}
	kept := make([]*model.Maintenance, 0, len(list))
	removed := false
	for _, m := range list {
		if m.ID == id {
			removed = true
			continue
		}
		kept = append(kept, m)
	}
	if !removed {
		return false, nil
	}
	return true, s.saveMaintenances(kept)
}

func (s *Store) saveMaintenances(list []*model.Maintenance) error {
	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	s.SetSetting(keyMaintenance, string(b))
	return nil
}

// MaintenanceAt 返回 ts 时刻处于维护中的窗口。
// 返回全局窗口（覆盖全部节点，可能为 nil）与按节点 ID 索引的窗口，
// 调用方据此判断某个节点当前是否"计划维护中"。
// 全局窗口存在时 nodeIDs 里的条目仍然有效（用于展示更具体的标题）。
func (s *Store) MaintenanceAt(ts int64) (global *model.Maintenance, nodeIDs map[string]*model.Maintenance) {
	nodeIDs = map[string]*model.Maintenance{}
	for _, m := range s.Maintenances() {
		if m == nil || !m.ActiveAt(ts) {
			continue
		}
		if len(m.NodeIDs) == 0 {
			if global == nil {
				global = m
			}
			continue
		}
		for _, id := range m.NodeIDs {
			nodeIDs[id] = m
		}
	}
	return global, nodeIDs
}

// PruneMaintenance 清理已经结束很久的窗口，避免列表无限增长。
// 保留 grace 秒内的历史，便于"刚刚结束的维护"仍能在时间线里查到。
func (s *Store) PruneMaintenance(now, grace int64) int {
	s.spMu.Lock()
	defer s.spMu.Unlock()
	list, err := s.maintenancesLocked()
	if err != nil {
		return 0 // 配置损坏时不动它，交给管理员排查
	}
	kept := make([]*model.Maintenance, 0, len(list))
	for _, m := range list {
		if m.EndAt+grace < now {
			continue
		}
		kept = append(kept, m)
	}
	if len(kept) == len(list) {
		return 0
	}
	if err := s.saveMaintenances(kept); err != nil {
		return 0
	}
	return len(list) - len(kept)
}

// ---------- 人工事件 ----------

// Incidents 返回全部人工事件，按开始时间倒序（最近的在前）。
func (s *Store) Incidents() []*model.Incident {
	s.spMu.Lock()
	defer s.spMu.Unlock()
	out, _ := s.incidentsLocked()
	return out
}

func (s *Store) incidentsLocked() ([]*model.Incident, error) {
	var out []*model.Incident
	raw := s.GetSetting(keyIncidents)
	if raw == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("人工事件配置损坏: %w", err)
	}
	kept := out[:0]
	for _, it := range out {
		if it == nil || it.ID == "" || it.StartAt <= 0 {
			continue
		}
		kept = append(kept, it)
	}
	out = kept
	sort.Slice(out, func(i, j int) bool { return out[i].StartAt > out[j].StartAt })
	return out, nil
}

// UpsertIncident 新增或更新一条人工事件。
func (s *Store) UpsertIncident(it *model.Incident) error {
	if it == nil || it.ID == "" {
		return errors.New("人工事件缺少 ID")
	}
	it.Title = sanitize.Text(it.Title, 200)
	it.Body = sanitize.Text(it.Body, 4000)
	if it.Title == "" {
		return errors.New("人工事件缺少标题")
	}
	if !model.ValidLevel(it.Level) {
		it.Level = model.LevelInfo
	}
	if it.StartAt <= 0 {
		it.StartAt = time.Now().Unix()
	}
	if it.EndAt != 0 && it.EndAt < it.StartAt {
		return errors.New("事件结束时间不能早于开始时间")
	}
	if it.CreatedAt <= 0 {
		it.CreatedAt = time.Now().Unix()
	}
	s.spMu.Lock()
	defer s.spMu.Unlock()
	list, err := s.incidentsLocked()
	if err != nil {
		return err
	}
	replaced := false
	for i, cur := range list {
		if cur.ID == it.ID {
			if cur.CreatedAt > 0 {
				it.CreatedAt = cur.CreatedAt
			}
			list[i] = it
			replaced = true
			break
		}
	}
	if !replaced {
		list = append(list, it)
	}
	return s.saveIncidents(list)
}

// DeleteIncident 删除一条人工事件。
func (s *Store) DeleteIncident(id string) (bool, error) {
	s.spMu.Lock()
	defer s.spMu.Unlock()
	list, err := s.incidentsLocked()
	if err != nil {
		return false, err
	}
	kept := make([]*model.Incident, 0, len(list))
	removed := false
	for _, it := range list {
		if it.ID == id {
			removed = true
			continue
		}
		kept = append(kept, it)
	}
	if !removed {
		return false, nil
	}
	return true, s.saveIncidents(kept)
}

func (s *Store) saveIncidents(list []*model.Incident) error {
	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	s.SetSetting(keyIncidents, string(b))
	return nil
}

// PruneIncidents 删除结束时间早于截止点的事件。
// 进行中的（EndAt == 0）永不清理。
func (s *Store) PruneIncidents(before int64) int {
	s.spMu.Lock()
	defer s.spMu.Unlock()
	list, err := s.incidentsLocked()
	if err != nil {
		return 0
	}
	kept := make([]*model.Incident, 0, len(list))
	for _, it := range list {
		if it.EndAt != 0 && it.EndAt < before {
			continue
		}
		kept = append(kept, it)
	}
	if len(kept) == len(list) {
		return 0
	}
	if err := s.saveIncidents(kept); err != nil {
		return 0
	}
	return len(list) - len(kept)
}

// ---------- 工具 ----------

// Groups 返回全部已使用的分组名（去重、升序）。
// 后台的"维护窗口/事件影响范围"选择器需要它做下拉选项，
// 不能让前端自己从节点列表里拼 —— 那样空分组会被拼成空字符串选项。
func (s *Store) Groups() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := make(map[string]struct{}, len(s.nodes))
	out := make([]string, 0, 8)
	for _, n := range s.nodes {
		if n == nil {
			continue
		}
		g := strings.TrimSpace(n.Group)
		if g == "" {
			continue
		}
		if _, dup := seen[g]; dup {
			continue
		}
		seen[g] = struct{}{}
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

// VisibleGroup 返回节点在公开页上的归类：分组为空时退回一个稳定占位名，
// 避免同一批无分组节点被 JS 归到不同的 key 下（"" 与 undefined 是两个键）。
func VisibleGroup(g string) string {
	g = strings.TrimSpace(g)
	if g == "" {
		return "节点"
	}
	return g
}

// ---------- 公开页节点句柄 ----------

// keyPublicSalt 是派生公开句柄的盐值键。
const keyPublicSalt = "status_salt"

// publicSalt 返回（必要时生成并持久化）公开句柄盐值。
//
// 为什么需要盐：README 的脱敏原则明确承诺公开页「不含节点 ID」，
// 但节点详情页又需要一个可点击的稳定标识。直接下发内部 ID 会让
// 「资产台账内部 ID」与「公开页面」产生可关联性；用盐值派生的一次性
// 句柄既可点击、又推不回内部 ID。
//
// 盐值必须持久化：否则每次重启句柄都变，书签与分享链接全部失效。
func (s *Store) publicSalt() string {
	if v := s.GetSetting(keyPublicSalt); v != "" {
		return v
	}
	s.mu.Lock()
	// 双重检查：不加这一步，并发首请求会各生成一个盐值，
	// 后写入的覆盖前者，已发出的句柄立刻失效（详情页 404）。
	if v := s.settings[keyPublicSalt]; v != "" {
		s.mu.Unlock()
		return v
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败极罕见（fd 耗尽等）。此时仍要保证进程内稳定，
		// 否则同一节点每次请求都换句柄，前端详情页会一直 404。
		// 退化后的盐值可预测，必须留下明确日志供运维排查。
		log.Printf("crypto/rand 失败，公开句柄盐值退化为时间派生值: %v", err)
		b = []byte(strconv.FormatInt(time.Now().UnixNano(), 10))
	}
	v := hex.EncodeToString(b)
	if s.settings == nil {
		s.settings = map[string]string{}
	}
	s.settings[keyPublicSalt] = v
	s.mu.Unlock()
	// 同步落盘：盐值丢失等于所有公开链接失效。
	// 走 RequestSave 的 2 秒合并窗口时，这段窗口内崩溃就会让盐值消失。
	if err := s.Save(); err != nil {
		log.Printf("保存公开句柄盐值失败（重启后公开链接可能失效）: %v", err)
	}
	return v
}

// PublicHandle 返回节点在公开页上的不透明句柄（h 前缀 + 12 位十六进制）。
func (s *Store) PublicHandle(nodeID string) string {
	sum := sha256.Sum256([]byte(s.publicSalt() + "|" + nodeID))
	return "h" + hex.EncodeToString(sum[:])[:12]
}

// NodeByPublicHandle 按公开句柄反查节点。
// 节点规模在百台量级，线性扫描比维护反向索引更不容易出错，
// 也不会因为漏更新索引而在改节点后出现"句柄失效"的隐蔽 bug。
func (s *Store) NodeByPublicHandle(handle string) (*model.Node, bool) {
	if handle == "" {
		return nil, false
	}
	for _, n := range s.Nodes() {
		if s.PublicHandle(n.ID) == handle {
			return n, true
		}
	}
	return nil, false
}

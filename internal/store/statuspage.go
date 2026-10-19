package store

import (
	"encoding/json"
	"sort"
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
	var out []*model.Maintenance
	raw := s.GetSetting(keyMaintenance)
	if raw == "" {
		return out
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
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
	return out
}

// UpsertMaintenance 新增或更新一个维护窗口。
// Title 为空时用一个稳定的占位标题：公开页要显示"为什么在维护"，
// 空标题会让用户只看到一个无解释的徽章。
func (s *Store) UpsertMaintenance(m *model.Maintenance) {
	if m == nil {
		return
	}
	m.Title = sanitize.Text(m.Title, 128)
	m.Reason = sanitize.Text(m.Reason, 500)
	if m.Title == "" {
		m.Title = "计划维护"
	}
	if m.EndAt <= m.StartAt {
		return // 时间非法的窗口永不结束，直接拒绝写入
	}
	list := s.Maintenances()
	replaced := false
	for i, cur := range list {
		if cur.ID == m.ID {
			list[i] = m
			replaced = true
			break
		}
	}
	if !replaced {
		list = append(list, m)
	}
	s.saveMaintenances(list)
}

// DeleteMaintenance 删除维护窗口。
func (s *Store) DeleteMaintenance(id string) bool {
	list := s.Maintenances()
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
		return false
	}
	s.saveMaintenances(kept)
	return true
}

func (s *Store) saveMaintenances(list []*model.Maintenance) {
	b, err := json.Marshal(list)
	if err != nil {
		return
	}
	s.SetSetting(keyMaintenance, string(b))
}

// MaintenanceAt 返回 ts 时刻处于维护中的窗口。
// 返回两个结果：全局窗口（覆盖全部节点）与按节点 ID 索引的窗口，
// 调用方据此判断某个节点当前是否"计划维护中"。
// global 为真时 nodeIDs 里的条目仍然有效（用于展示更具体的标题）。
func (s *Store) MaintenanceAt(ts int64) (global bool, nodeIDs map[string]*model.Maintenance) {
	nodeIDs = map[string]*model.Maintenance{}
	for _, m := range s.Maintenances() {
		if !m.ActiveAt(ts) {
			continue
		}
		if len(m.NodeIDs) == 0 {
			global = true
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
	list := s.Maintenances()
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
	s.saveMaintenances(kept)
	return len(list) - len(kept)
}

// ---------- 人工事件 ----------

// Incidents 返回全部人工事件，按开始时间倒序（最近的在前）。
func (s *Store) Incidents() []*model.Incident {
	var out []*model.Incident
	raw := s.GetSetting(keyIncidents)
	if raw == "" {
		return out
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
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
	return out
}

// UpsertIncident 新增或更新一条人工事件。
func (s *Store) UpsertIncident(it *model.Incident) {
	if it == nil || it.ID == "" {
		return
	}
	it.Title = sanitize.Text(it.Title, 200)
	it.Body = sanitize.Text(it.Body, 4000)
	if it.Title == "" {
		return
	}
	if !model.ValidLevel(it.Level) {
		it.Level = model.LevelInfo
	}
	if it.StartAt <= 0 {
		it.StartAt = time.Now().Unix()
	}
	if it.EndAt != 0 && it.EndAt < it.StartAt {
		it.EndAt = 0 // 结束早于开始按"进行中"处理，而不是产生负时长
	}
	if it.CreatedAt <= 0 {
		it.CreatedAt = time.Now().Unix()
	}
	list := s.Incidents()
	replaced := false
	for i, cur := range list {
		if cur.ID == it.ID {
			list[i] = it
			replaced = true
			break
		}
	}
	if !replaced {
		list = append(list, it)
	}
	s.saveIncidents(list)
}

// DeleteIncident 删除一条人工事件。
func (s *Store) DeleteIncident(id string) bool {
	list := s.Incidents()
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
		return false
	}
	s.saveIncidents(kept)
	return true
}

func (s *Store) saveIncidents(list []*model.Incident) {
	b, err := json.Marshal(list)
	if err != nil {
		return
	}
	s.SetSetting(keyIncidents, string(b))
}

// PruneIncidents 删除结束时间早于截止点的事件。
// 进行中的（EndAt == 0）永不清理。
func (s *Store) PruneIncidents(before int64) int {
	list := s.Incidents()
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
	s.saveIncidents(kept)
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

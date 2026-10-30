package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/motao123/rollcall/internal/model"
)

// ConfigBackupVersion 是配置备份格式版本。
// v2 起在独立的 tokens 表里保存节点上报令牌：Node.Token 标了 json:"-"，
// 不单独导出的话还原后会生成新令牌，全部 Agent 立刻 401。
// v3 起携带监控聚合历史（HistoryGz，gzip 的 historySnapshot）：
// 控制端磁盘故障不再丢掉 365 天可用率/负载历史。历史以整体替换方式还原，
// 原始 24h 环形不包含在内——它会在恢复后的 24 小时内随上报自愈。
const ConfigBackupVersion = 3

// ConfigBackup 是可移植的配置快照：台账、探测目标、规则、静默、续费与设置。
// v2 及以下不含监控历史；v3 的 HistoryGz 是 encodeSnapshot(historySnapshot) 的
// 原样字节——与 data/history.json 同一编解码路径，不做有损的投影往返。
// 不含告警记录与通知队列，避免备份文件随运行时间无限增长。
type ConfigBackup struct {
	Version  int                `json:"version"`
	SavedAt  int64              `json:"savedAt"`
	Nodes    []*model.Node      `json:"nodes"`
	Services []*model.Service   `json:"services"`
	Rules    []*model.AlertRule `json:"rules"`
	Silences []*model.Silence   `json:"silences,omitempty"`
	Renewals []*model.Renewal   `json:"renewals,omitempty"`
	Settings map[string]string  `json:"settings,omitempty"`
	// Tokens 是 nodeID -> 上报令牌。整个备份在落盘/传输前会用密码加密。
	Tokens map[string]string `json:"tokens,omitempty"`
	// HistoryGz 是三级聚合历史的 gzip 快照（v3 起）。含节点→桶序列，
	// 不含任何凭据；还原时按 schema 版本校验后整体替换。
	HistoryGz []byte `json:"historyGz,omitempty"`
}

// ExportConfig 导出配置（含节点上报令牌，因此调用方必须加密后再传输/落盘）。
func (s *Store) ExportConfig() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := ConfigBackup{
		Version:  ConfigBackupVersion,
		SavedAt:  time.Now().Unix(),
		Nodes:    make([]*model.Node, 0, len(s.nodes)),
		Settings: make(map[string]string, len(s.settings)),
		Tokens:   make(map[string]string, len(s.nodes)),
	}
	for _, n := range s.nodes {
		cp := *n
		out.Nodes = append(out.Nodes, &cp)
		if cp.Token != "" {
			out.Tokens[cp.ID] = cp.Token
		}
	}
	for _, sv := range s.services {
		cp := *sv
		out.Services = append(out.Services, &cp)
	}
	for _, r := range s.rules {
		cp := *r
		cp.NodeIDs = append([]string(nil), r.NodeIDs...)
		cp.Groups = append([]string(nil), r.Groups...)
		cp.ServiceIDs = append([]string(nil), r.ServiceIDs...)
		out.Rules = append(out.Rules, &cp)
	}
	for _, sl := range s.notifySilent {
		cp := *sl
		out.Silences = append(out.Silences, &cp)
	}
	for _, rn := range s.renewals {
		// 节点已删除的续费记录不再导出：导入侧同样会跳过这类记录
		// （见 ImportConfig），两边保持一致，避免"导出→导入"静默丢数据。
		if _, ok := s.nodes[rn.NodeID]; !ok {
			continue
		}
		cp := *rn
		out.Renewals = append(out.Renewals, &cp)
	}
	for k, v := range s.settings {
		// 会话吊销列表属于运行态安全状态，导入别人的值可能让已登出的
		// 令牌重新可用；始终保留本机当前值。
		if k == "revoked_sessions" {
			continue
		}
		out.Settings[k] = v
	}
	sort.Slice(out.Nodes, func(i, j int) bool { return out.Nodes[i].ID < out.Nodes[j].ID })
	sort.Slice(out.Services, func(i, j int) bool { return out.Services[i].ID < out.Services[j].ID })
	sort.Slice(out.Rules, func(i, j int) bool { return out.Rules[i].ID < out.Rules[j].ID })
	sort.Slice(out.Silences, func(i, j int) bool { return out.Silences[i].Scope < out.Silences[j].Scope })
	sort.Slice(out.Renewals, func(i, j int) bool { return out.Renewals[i].ID < out.Renewals[j].ID })
	// v3：聚合历史快照。与 SaveHistory 用同一个 historySnapshot 形状，
	// 还原时走同一条 loadBuckets 路径，不做有损投影。当前持有 RLock，
	// 这里只做只读遍历与序列化。
	histSnap := historySnapshot{
		SchemaVersion: historySchemaVersion,
		SavedAt:       out.SavedAt,
		Nodes:         make(map[string]*historyBucketSnapshot, len(s.history)),
	}
	for id, h := range s.history {
		if h == nil {
			continue
		}
		histSnap.Nodes[id] = &historyBucketSnapshot{
			LastTS: h.lastTS,
			M1:     h.m1.slice(),
			M5:     h.m5.slice(),
			H1:     h.h1.slice(),
		}
	}
	if len(histSnap.Nodes) > 0 {
		if hraw, err := json.Marshal(&histSnap); err == nil {
			if gz, gerr := encodeSnapshot(hraw); gerr == nil {
				out.HistoryGz = gz
			} else {
				// 历史打包失败不阻断配置备份（配置仍是主价值），如实降级为 v2 形状。
				out.HistoryGz = nil
			}
		}
	}
	return json.Marshal(out)
}

// ImportConfig 校验并替换当前配置。监控历史、告警与通知队列保持不变；
// 引用了已不存在节点/规则的活跃告警会被关闭，避免永久挂起。
func (s *Store) ImportConfig(raw []byte) error {
	var in ConfigBackup
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return fmt.Errorf("备份内容格式错误: %w", err)
	}
	if in.Version <= 0 || in.Version > ConfigBackupVersion {
		return fmt.Errorf("不支持的备份版本: %d", in.Version)
	}
	// v3 历史快照在改动任何状态之前先校验：schema 不兼容时整个导入失败，
	// 而不是"配置换成了备份的、历史还是本机的"这种半还原状态。
	var histSnap *historySnapshot
	if len(in.HistoryGz) > 0 {
		hraw, err := decodeSnapshot(in.HistoryGz)
		if err != nil {
			return fmt.Errorf("备份中的历史快照无法解码: %w", err)
		}
		histSnap = &historySnapshot{}
		if err := json.Unmarshal(hraw, histSnap); err != nil {
			return fmt.Errorf("备份中的历史快照格式错误: %w", err)
		}
		if histSnap.SchemaVersion > historySchemaVersion {
			return fmt.Errorf("备份历史 schema 版本 %d 高于当前支持的 %d；请先升级服务端再还原",
				histSnap.SchemaVersion, historySchemaVersion)
		}
	}

	nodes := make(map[string]*model.Node, len(in.Nodes))
	for _, n := range in.Nodes {
		if n == nil || strings.TrimSpace(n.ID) == "" || strings.TrimSpace(n.Name) == "" {
			return errors.New("备份中存在缺少 id/name 的节点")
		}
		if _, dup := nodes[n.ID]; dup {
			return fmt.Errorf("备份中节点 ID 重复: %s", n.ID)
		}
		cp := *n
		if cp.Token == "" {
			// v1 备份没有令牌表；v2 也允许缺失。没有令牌的节点导入后
			// Agent 将无法上报，生成一个新令牌让用户在面板重新接入。
			cp.Token = strings.TrimSpace(in.Tokens[cp.ID])
		}
		if cp.Token == "" {
			cp.Token = NewToken()
		}
		nodes[cp.ID] = &cp
	}
	services := make(map[string]*model.Service, len(in.Services))
	for _, sv := range in.Services {
		if sv == nil || strings.TrimSpace(sv.ID) == "" || strings.TrimSpace(sv.Name) == "" {
			return errors.New("备份中存在缺少 id/name 的服务")
		}
		if _, ok := nodes[sv.NodeID]; !ok {
			return fmt.Errorf("服务 %s 引用了不存在的节点 %s", sv.Name, sv.NodeID)
		}
		if _, dup := services[sv.ID]; dup {
			return fmt.Errorf("备份中服务 ID 重复: %s", sv.ID)
		}
		cp := *sv
		services[cp.ID] = &cp
	}
	rules := make(map[string]*model.AlertRule, len(in.Rules))
	for _, r := range in.Rules {
		if r == nil || strings.TrimSpace(r.ID) == "" || strings.TrimSpace(r.Name) == "" {
			return errors.New("备份中存在缺少 id/name 的规则")
		}
		if _, dup := rules[r.ID]; dup {
			return fmt.Errorf("备份中规则 ID 重复: %s", r.ID)
		}
		// 规则的作用域必须指向本备份里存在的节点/服务，否则导入后会留下
		// 永不触发的悬空规则（界面显示正常，但实际什么也不监控）。
		for _, id := range r.NodeIDs {
			if _, ok := nodes[id]; !ok {
				return fmt.Errorf("规则 %s 引用了不存在的节点 %s", r.Name, id)
			}
		}
		for _, id := range r.ServiceIDs {
			if _, ok := services[id]; !ok {
				return fmt.Errorf("规则 %s 引用了不存在的服务 %s", r.Name, id)
			}
		}
		cp := *r
		rules[cp.ID] = &cp
	}
	silences := make(map[string]*model.Silence, len(in.Silences))
	for _, sl := range in.Silences {
		if sl == nil || sl.Scope == "" {
			continue
		}
		cp := *sl
		silences[cp.Scope] = &cp
	}
	renewals := make([]*model.Renewal, 0, len(in.Renewals))
	for _, rn := range in.Renewals {
		if rn == nil || rn.ID == "" {
			continue
		}
		if _, ok := nodes[rn.NodeID]; !ok {
			continue
		}
		cp := *rn
		renewals = append(renewals, &cp)
	}
	settings := in.Settings
	if settings == nil {
		settings = map[string]string{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.nodes = nodes
	// ★ 导入的节点必须撤销墓碑：本进程内删过同一个 ID 时，墓碑会让
	// AddMetrics 直接丢弃上报（200 但仍不写数据），表现为"节点回来了但永远没数据"。
	// 备份恢复正是最常走的路径（误删 → 导入自动备份）。
	for id := range nodes {
		delete(s.dropped, id)
	}
	s.services = services
	s.rules = rules
	s.notifySilent = silences
	s.renewals = renewals
	// 设置项按"覆盖同名键"合并，而不是整体替换：备份里没有的键保留本机值。
	// 这样旧备份不会清掉新版本才有的设置项（否则升级后导入一份旧备份，
	// 新功能的配置会静默回落到默认值）。UI 文案按这个语义描述。
	for k, v := range settings {
		if k == "revoked_sessions" {
			continue
		}
		s.settings[k] = v
	}
	// v3：整体替换聚合历史。口径（拍板记录）：
	//   · 还原 = 用备份里的历史**整体替换**本机聚合——不做按节点合并，
	//     合并会让"备份时间点之后本机新聚合的桶"与"备份里的桶"交错，
	//     可用率曲线从此无法解释；宁要一段诚实的空窗，不要一段假数据。
	//   · 备份里没有的节点 → 历史一并清掉（与节点集合保持一致）。
	//   · 原始 24h 环形不在备份内：恢复后 24 小时内随上报自愈，
	//     换取备份体积不随环大小膨胀。
	//   · 本机当前聚合被覆盖前不做合并确认——恢复走显式 Confirm + 密码，
	//     且备份/恢复是成对的运维动作。
	if histSnap != nil {
		s.history = make(map[string]*nodeHistory, len(histSnap.Nodes))
		for id, nh := range histSnap.Nodes {
			if nh == nil {
				continue
			}
			h := newNodeHistory()
			h.lastTS = nh.LastTS
			s.loadBuckets(h.m1, nh.M1, false)
			s.loadBuckets(h.m5, nh.M5, true)
			s.loadBuckets(h.h1, nh.H1, true)
			s.history[id] = h
		}
		s.historyDirty = true
	}
	// 导入会重建节点与 series：把分片写状态整体重置，
	// 让下一次 Save 把**全部**节点的历史重写到 data/series 下。
	// 不重置的话，磁盘上残留的分片会被当成"已保存"，
	// 导入后历史图会显示备份里的旧数据、甚至已删除节点的数据。
	s.seriesSeq = make(map[string]uint64, len(s.series))
	s.seriesSaved = make(map[string]uint64, len(s.series))
	for id, r := range s.series {
		if r != nil && r.n > 0 {
			s.seriesSeq[id] = 1
		}
	}

	// 清理不再存在的节点运行态数据，避免导出→导入后旧节点幽灵般残留。
	// 遍历并集：series 在 UpsertNode 时就建 Ring，expirySent 也可能先于 hosts
	// 存在，只以 hosts 为遍历源会漏掉这些孤儿数据。
	stale := map[string]bool{}
	for id := range s.hosts {
		stale[id] = true
	}
	for id := range s.series {
		stale[id] = true
	}
	for id := range s.traffic {
		stale[id] = true
	}
	for id := range s.expirySent {
		stale[id] = true
	}
	for id := range stale {
		if _, ok := nodes[id]; ok {
			continue
		}
		delete(s.hosts, id)
		delete(s.series, id)
		delete(s.traffic, id)
		delete(s.expirySent, id)
	}
	for id := range s.svcRes {
		if _, ok := services[id]; !ok {
			delete(s.svcRes, id)
		}
	}
	now := time.Now().Unix()
	for _, a := range s.alerts {
		if a.ResolvedAt != 0 {
			continue
		}
		_, hasRule := rules[a.RuleID]
		_, hasNode := nodes[a.NodeID]
		hasSvc := true
		if a.ServiceID != "" {
			_, hasSvc = services[a.ServiceID]
		}
		if !hasRule || !hasNode || !hasSvc {
			a.ResolvedAt = now
		}
	}
	s.RequestSave()
	return nil
}

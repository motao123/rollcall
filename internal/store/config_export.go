package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/motao/rollcall/internal/model"
)

// ConfigBackupVersion 是配置备份格式版本。
// v2 起在独立的 tokens 表里保存节点上报令牌：Node.Token 标了 json:"-"，
// 不单独导出的话还原后会生成新令牌，全部 Agent 立刻 401。
const ConfigBackupVersion = 2

// ConfigBackup 是可移植的配置快照：台账、探测目标、规则、静默、续费与设置。
// 不含监控历史与告警记录，避免备份文件随运行时间无限增长。
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
		cp := *sv
		services[cp.ID] = &cp
	}
	rules := make(map[string]*model.AlertRule, len(in.Rules))
	for _, r := range in.Rules {
		if r == nil || strings.TrimSpace(r.ID) == "" || strings.TrimSpace(r.Name) == "" {
			return errors.New("备份中存在缺少 id/name 的规则")
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
	s.services = services
	s.rules = rules
	s.notifySilent = silences
	s.renewals = renewals
	for k, v := range settings {
		s.settings[k] = v
	}
	// 清理不再存在的节点运行态数据，避免导出→导入后旧节点幽灵般残留。
	for id := range s.hosts {
		if _, ok := nodes[id]; !ok {
			delete(s.hosts, id)
			delete(s.series, id)
			delete(s.traffic, id)
			delete(s.expirySent, id)
		}
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

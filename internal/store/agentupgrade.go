package store

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/motao123/rollcall/internal/model"
)

const (
	keyAgentUpgrades = "agentUpgrades"
	agentUpgradeMax  = 30
)

// AgentUpgrades 返回全部升级批次，最近创建的在前。
func (s *Store) AgentUpgrades() []*model.AgentUpgradeBatch {
	s.spMu.Lock()
	defer s.spMu.Unlock()
	out, _ := s.agentUpgradesLocked()
	return out
}

func (s *Store) agentUpgradesLocked() ([]*model.AgentUpgradeBatch, error) {
	var out []*model.AgentUpgradeBatch
	raw := s.GetSetting(keyAgentUpgrades)
	if raw == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("Agent 升级批次配置损坏: %w", err)
	}
	kept := out[:0]
	for _, b := range out {
		if b == nil || b.ID == "" || len(b.NodeIDs) == 0 {
			continue
		}
		kept = append(kept, b)
	}
	out = kept
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, nil
}

// UpsertAgentUpgrade 保存一个升级批次，并把总数限制在 agentUpgradeMax。
func (s *Store) UpsertAgentUpgrade(b *model.AgentUpgradeBatch) error {
	if b == nil || b.ID == "" {
		return fmt.Errorf("升级批次缺少 ID")
	}
	if b.CreatedAt <= 0 {
		b.CreatedAt = time.Now().Unix()
	}
	if b.Before == nil {
		b.Before = map[string]string{}
	}
	s.spMu.Lock()
	defer s.spMu.Unlock()
	list, err := s.agentUpgradesLocked()
	if err != nil {
		return err
	}
	replaced := false
	for i, cur := range list {
		if cur.ID == b.ID {
			list[i] = b
			replaced = true
			break
		}
	}
	if !replaced {
		list = append(list, b)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt > list[j].CreatedAt })
	if len(list) > agentUpgradeMax {
		list = list[:agentUpgradeMax]
	}
	raw, err := json.Marshal(list)
	if err != nil {
		return err
	}
	s.SetSetting(keyAgentUpgrades, string(raw))
	return nil
}

// DeleteAgentUpgrade 删除一个批次记录。
func (s *Store) DeleteAgentUpgrade(id string) (bool, error) {
	s.spMu.Lock()
	defer s.spMu.Unlock()
	list, err := s.agentUpgradesLocked()
	if err != nil {
		return false, err
	}
	kept := make([]*model.AgentUpgradeBatch, 0, len(list))
	removed := false
	for _, b := range list {
		if b.ID == id {
			removed = true
			continue
		}
		kept = append(kept, b)
	}
	if !removed {
		return false, nil
	}
	raw, err := json.Marshal(kept)
	if err != nil {
		return false, err
	}
	s.SetSetting(keyAgentUpgrades, string(raw))
	return true, nil
}

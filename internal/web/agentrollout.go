package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/sanitize"
	"github.com/motao123/rollcall/internal/store"
	"github.com/motao123/rollcall/internal/version"
)

// agentUpgradeView 在批次记录上补充由当前 Agent 版本推导的进度。
type agentUpgradeView struct {
	model.AgentUpgradeBatch
	Targets []model.AgentUpgradeTarget `json:"targets"`
	Done    int                        `json:"done"`
	Total   int                        `json:"total"`
}

// buildAgentUpgradeView 按节点当前版本推导每台机器的升级状态。
//
// 判定标准是"版本与创建批次时不同"而不是"等于目标版本"：
// Agent 可能直接升级到更新的版本，这同样意味着这台机器已经不在旧版本上。
func (s *Server) buildAgentUpgradeView(b *model.AgentUpgradeBatch) agentUpgradeView {
	v := agentUpgradeView{AgentUpgradeBatch: *b, Total: len(b.NodeIDs)}
	v.Targets = make([]model.AgentUpgradeTarget, 0, len(b.NodeIDs))
	for _, id := range b.NodeIDs {
		t := model.AgentUpgradeTarget{ID: id, Before: b.Before[id]}
		n, ok := s.st.Node(id)
		if !ok {
			t.State = "missing"
			v.Targets = append(v.Targets, t)
			continue
		}
		t.Name = n.Name
		t.Current = n.AgentVersion
		if n.AgentVersion != "" && n.AgentVersion != t.Before {
			t.State = "done"
			v.Done++
		} else {
			t.State = "pending"
		}
		v.Targets = append(v.Targets, t)
	}
	return v
}

func (s *Server) handleAdminAgentUpgrades(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list := s.st.AgentUpgrades()
		out := make([]agentUpgradeView, 0, len(list))
		for _, b := range list {
			out = append(out, s.buildAgentUpgradeView(b))
		}
		writeJSON(w, 200, map[string]any{
			"items":         out,
			"serverVersion": version.Version,
		})

	case http.MethodPost:
		body := http.MaxBytesReader(w, r.Body, 64<<10)
		dec := json.NewDecoder(body)
		dec.DisallowUnknownFields()
		var req struct {
			NodeIDs       []string `json:"nodeIds"`
			TargetVersion string   `json:"targetVersion"`
			Note          string   `json:"note"`
		}
		if err := dec.Decode(&req); err != nil {
			fail(w, 400, "bad json: "+err.Error())
			return
		}
		if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
			fail(w, 400, "请求体只能包含一个 JSON 对象")
			return
		}
		req.NodeIDs = dedupeStrings(trimListSpace(req.NodeIDs))
		if len(req.NodeIDs) == 0 {
			fail(w, 400, "至少选择一个节点")
			return
		}
		if len(req.NodeIDs) > 500 {
			fail(w, 400, "单次最多 500 个节点")
			return
		}
		req.TargetVersion = sanitize.Text(req.TargetVersion, 40)
		if req.TargetVersion == "" {
			req.TargetVersion = version.Version
		}
		b := &model.AgentUpgradeBatch{
			ID:            store.NewID("up"),
			TargetVersion: req.TargetVersion,
			Note:          sanitize.Text(req.Note, 200),
			CreatedAt:     time.Now().Unix(),
			NodeIDs:       req.NodeIDs,
			Before:        map[string]string{},
		}
		for _, id := range req.NodeIDs {
			n, ok := s.st.Node(id)
			if !ok {
				fail(w, 400, "节点不存在: "+id)
				return
			}
			b.Before[id] = strings.TrimSpace(n.AgentVersion)
		}
		if err := s.st.UpsertAgentUpgrade(b); err != nil {
			fail(w, 500, "保存升级批次失败: "+err.Error())
			return
		}
		writeJSON(w, 201, map[string]any{"item": s.buildAgentUpgradeView(b)})

	default:
		fail(w, 405, "method not allowed")
	}
}

func (s *Server) handleAdminAgentUpgradeByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		fail(w, 405, "method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/admin/agent-upgrades/")
	if id == "" || strings.Contains(id, "/") {
		fail(w, 400, "missing batch id")
		return
	}
	ok, err := s.st.DeleteAgentUpgrade(id)
	if err != nil {
		fail(w, 500, "删除失败: "+err.Error())
		return
	}
	if !ok {
		fail(w, 404, "batch not found")
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": id})
}

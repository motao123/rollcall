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
)

// handleAdminNodesBatch 节点批量操作。
//
// 一次请求服务端循环改完再统一落盘，而不是让前端发 N 个 PUT：
// 前端逐台发请求中途失败会留下"改了一半"的台账，
// 而且每台一次快照落盘，40 台改分组会写 40 次盘。
func (s *Server) handleAdminNodesBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, 405, "method not allowed")
		return
	}
	body := http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	var req struct {
		IDs    []string `json:"ids"`
		Action string   `json:"action"` // group | provider | region | expire | currency | delete
		Value  string   `json:"value"`
	}
	if err := dec.Decode(&req); err != nil {
		fail(w, 400, "bad json: "+err.Error())
		return
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		fail(w, 400, "请求体只能包含一个 JSON 对象")
		return
	}
	if len(req.IDs) == 0 {
		fail(w, 400, "ids 不能为空")
		return
	}
	if len(req.IDs) > 500 {
		fail(w, 400, "单次最多操作 500 个节点")
		return
	}
	// 重复 ID 会让 delete 被计数多次、分组被写多次；去重后再校验。
	req.IDs = dedupeStrings(trimListSpace(req.IDs))
	if len(req.IDs) == 0 {
		fail(w, 400, "ids 不能为空")
		return
	}
	req.Value = sanitize.Text(req.Value, 128)

	// 先整体校验再动手：改到一半发现某个 ID 不存在，
	// 已经改过的无法回滚（台账没有事务）。
	nodes := make([]*model.Node, 0, len(req.IDs))
	for _, id := range req.IDs {
		n, ok := s.st.Node(id)
		if !ok {
			fail(w, 400, "节点不存在: "+id)
			return
		}
		nodes = append(nodes, n)
	}

	switch req.Action {
	case "group":
		if req.Value == "" {
			fail(w, 400, "分组名不能为空")
			return
		}
	case "provider", "region":
		// 这两个动作在下面的变更循环里有实现，但校验 switch 漏了分支，
		// 会直接落到 default 报"未知操作"。这里与单节点编辑保持同样的长度上限。
		if len([]rune(req.Value)) > 64 {
			fail(w, 400, "value 不能超过 64 字符")
			return
		}
	case "expire":
		if req.Value != "" && !validDate(req.Value) {
			fail(w, 400, "到期日格式应为 YYYY-MM-DD")
			return
		}
	case "currency":
		if req.Value != "USD" && req.Value != "CNY" {
			fail(w, 400, "币种只支持 USD / CNY")
			return
		}
	case "delete":
	default:
		fail(w, 400, "未知操作: "+req.Action)
		return
	}

	changed := 0
	for _, old := range nodes {
		if req.Action == "delete" {
			if s.st.DeleteNode(old.ID) {
				changed++
			}
			continue
		}
		if s.st.UpdateNode(old.ID, func(n *model.Node) {
			switch req.Action {
			case "group":
				n.Group = req.Value
			case "provider":
				n.Provider = req.Value
			case "region":
				n.Region = req.Value
			case "expire":
				n.ExpireAt = req.Value
			case "currency":
				n.Currency = req.Value
			}
		}) {
			changed++
		}
	}
	writeJSON(w, 200, map[string]any{"changed": changed, "action": req.Action})
}

// validDate 校验 YYYY-MM-DD。台账里的到期日一直是这个格式，
// 批量写入混入别的格式会让"30 天内到期"的筛选静默失效。
// 必须按真实日历校验：手写 1..31 会放过 2026-02-31，
// 而 daysUntil 解析失败后该节点会从"即将到期"列表里静默消失。
func validDate(s string) bool {
	t, err := time.Parse("2006-01-02", s)
	return err == nil && t.Format("2006-01-02") == s
}

// trimListSpace 去掉字符串列表里的空项与首尾空白。
// 节点多选框传来的 ID 列表可能混进空串，直接入库会让
// "维护窗口覆盖哪些节点"的判断出现永不匹配的幽灵条目。
func trimListSpace(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// dedupeStrings 按首次出现顺序去重，保留原顺序。
func dedupeStrings(in []string) []string {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]struct{}, len(in))
	out := in[:0]
	for _, v := range in {
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

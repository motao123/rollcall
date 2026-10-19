package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/motao/rollcall/internal/model"
	"github.com/motao/rollcall/internal/sanitize"
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
			s.st.DeleteNode(old.ID)
			changed++
			continue
		}
		updated := *old
		switch req.Action {
		case "group":
			updated.Group = req.Value
		case "provider":
			updated.Provider = req.Value
		case "region":
			updated.Region = req.Value
		case "expire":
			updated.ExpireAt = req.Value
		case "currency":
			updated.Currency = req.Value
		}
		s.st.UpsertNode(&updated)
		changed++
	}
	writeJSON(w, 200, map[string]any{"changed": changed, "action": req.Action})
}

// validDate 校验 YYYY-MM-DD。台账里的到期日一直是这个格式，
// 批量写入混入别的格式会让"30 天内到期"的筛选静默失效。
func validDate(s string) bool {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	for i, c := range s {
		if i == 4 || i == 7 {
			continue
		}
		if c < '0' || c > '9' {
			return false
		}
	}
	m := int(s[5]-'0')*10 + int(s[6]-'0')
	d := int(s[8]-'0')*10 + int(s[9]-'0')
	return m >= 1 && m <= 12 && d >= 1 && d <= 31
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

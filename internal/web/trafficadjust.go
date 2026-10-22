package web

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
)

// maxTrafficAdjustGB 是单次校正的上限（10 PB）。
// 取这个量级只为拦住明显的误输入（少写了小数点、把字节当 GB），
// 不是业务约束。
const maxTrafficAdjustGB = 10 * 1024 * 1024

// handleNodeTrafficAdjust 人工校正节点本周期已用流量。
//
//	POST /api/admin/nodes/{id}/traffic-adjust
//	{"usedGb":123.4}  或  {"usedBytes":132499741081}
//
// 为什么需要它：月中才配置重置日、或从别处迁移过来的节点，本周期已经跑掉的量
// 服务端根本没见过，不补录就永远和服务商面板对不上。写入后 Agent 的增量
// 会在该值之上继续累加，因此只需填一次。
func (s *Server) handleNodeTrafficAdjust(w http.ResponseWriter, r *http.Request, id string) {
	n, ok := s.st.Node(id)
	if !ok {
		fail(w, 404, "node not found")
		return
	}
	body := http.MaxBytesReader(w, r.Body, 4<<10)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	var in struct {
		UsedGB    *float64 `json:"usedGb"`
		UsedBytes *uint64  `json:"usedBytes"`
	}
	if err := dec.Decode(&in); err != nil {
		fail(w, 400, "bad json: "+err.Error())
		return
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		fail(w, 400, "请求体只能包含一个 JSON 对象")
		return
	}
	if in.UsedGB == nil && in.UsedBytes == nil {
		fail(w, 400, "必须提供 usedGb 或 usedBytes")
		return
	}
	if in.UsedGB != nil && in.UsedBytes != nil {
		fail(w, 400, "usedGb 与 usedBytes 只能二选一")
		return
	}

	var want uint64
	if in.UsedGB != nil {
		gb := *in.UsedGB
		if math.IsNaN(gb) || math.IsInf(gb, 0) || gb < 0 || gb > maxTrafficAdjustGB {
			fail(w, 400, "usedGb 必须在 0 到 10485760 之间")
			return
		}
		want = uint64(math.Round(gb * 1024 * 1024 * 1024))
	} else {
		want = *in.UsedBytes
	}

	before := s.st.TrafficUsage(id).Used
	usage, ok := s.st.AdjustTrafficUsed(id, want)
	if !ok {
		fail(w, 404, "node not found")
		return
	}
	s.auditTrafficAdjust(r, id, before, usage.Used)
	s.publishChange()
	writeJSON(w, 200, map[string]any{
		"nodeId":  n.ID,
		"before":  before,
		"after":   usage.Used,
		"traffic": usage,
	})
}

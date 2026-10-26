package web

// 批量建节点：把「N 台机器逐台点新建」压成一次粘贴。
//
// 场景是从其它探针/台账迁移——典型是一次 20–40 台。逐台在面板点「新增节点」
// 再逐台点「接入」复制命令，是最容易出错也最耗时的环节。
//
// 三条取舍：
//  1. **单条失败不打断整批**：返回 created + skipped，调用方能看清哪几行没进去。
//     迁移数据里重名/缺字段很常见，整批 400 只会让人逐条试。
//  2. **重名按跳过处理**：台账里「香港-1」这类名字重复是常态，不算错误。
//  3. **令牌一律服务端生成**，与单条创建同一条路径；响应直接带接入命令，
//     省去再逐台点一次「接入」。

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/store"
)

// maxImportNodes 限制单次导入条数。
//
// 上限存在的意义不是性能（40 条与 200 条都快），而是挡住误粘贴：
// 把一份几百行的表格贴进来，会在台账里留下一堆需要逐个删除的节点。
const maxImportNodes = 200

type importNodeSpec struct {
	Name           string   `json:"name"`
	Provider       string   `json:"provider,omitempty"`
	Region         string   `json:"region,omitempty"`
	Country        string   `json:"country,omitempty"`
	Group          string   `json:"group,omitempty"`
	Note           string   `json:"note,omitempty"`
	Price          float64  `json:"price,omitempty"`
	Currency       string   `json:"currency,omitempty"`
	ExpireAt       string   `json:"expireAt,omitempty"`
	TrafficQuotaGB float64  `json:"trafficQuotaGb,omitempty"`
	Tags           []string `json:"tags,omitempty"`
}

type importedNodeView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Command string `json:"command"` // 一行接入命令，直接可粘贴到目标机
}

type skippedNodeView struct {
	Index  int    `json:"index"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// handleAdminNodesImport 批量创建节点并返回每台的接入命令。
func (s *Server) handleAdminNodesImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	body := http.MaxBytesReader(w, r.Body, 256<<10)
	var req struct {
		Nodes []importNodeSpec `json:"nodes"`
	}
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if len(req.Nodes) == 0 {
		fail(w, http.StatusBadRequest, "nodes 不能为空")
		return
	}
	if len(req.Nodes) > maxImportNodes {
		fail(w, http.StatusBadRequest, "单次最多导入 200 个节点")
		return
	}

	// 已存在的名字（大小写与首尾空格不敏感）：迁移时重名几乎必然出现。
	taken := map[string]struct{}{}
	for _, n := range s.st.Nodes() {
		taken[strings.ToLower(strings.TrimSpace(n.Name))] = struct{}{}
	}

	created := make([]*model.Node, 0, len(req.Nodes))
	views := make([]importedNodeView, 0, len(req.Nodes))
	skipped := make([]skippedNodeView, 0)
	// 地址优先用「面板对外地址」：面板可能经反代/隧道访问，请求里的 Host
	// 未必是别人能连上的地址（例如从回环地址调 API 时拿到 127.0.0.1）。
	// 与单台「节点接入」弹窗的取值口径保持一致。
	base := strings.TrimRight(strings.TrimSpace(s.panelLink), "/")
	if base == "" {
		base = requestBaseURL(r)
	}

	for i, spec := range req.Nodes {
		n := &model.Node{
			Name:           spec.Name,
			Provider:       spec.Provider,
			Region:         spec.Region,
			Country:        spec.Country,
			Group:          spec.Group,
			Note:           spec.Note,
			Price:          spec.Price,
			Currency:       spec.Currency,
			ExpireAt:       spec.ExpireAt,
			TrafficQuotaGB: spec.TrafficQuotaGB,
			Tags:           spec.Tags,
		}
		if msg := validateNodeMeta(n); msg != "" {
			skipped = append(skipped, skippedNodeView{Index: i, Name: spec.Name, Reason: msg})
			continue
		}
		key := strings.ToLower(n.Name)
		if _, dup := taken[key]; dup {
			skipped = append(skipped, skippedNodeView{Index: i, Name: n.Name, Reason: "节点名已存在"})
			continue
		}
		taken[key] = struct{}{}
		n.ID = store.NewID("n")
		n.Token = store.NewToken()
		created = append(created, n)
		views = append(views, importedNodeView{
			ID:      n.ID,
			Name:    n.Name,
			Command: s.agentInstallCommand(base, n.ID, n.Token),
		})
	}

	// 整批一次落盘：逐条 UpsertNode 会为每台各写一次全量快照。
	s.st.UpsertNodes(created)

	// 界面据此立刻刷新节点列表；纯属提示，失败不影响导入结果。
	if len(created) > 0 {
		s.events.Publish()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"created": views,
		"skipped": skipped,
	})
}

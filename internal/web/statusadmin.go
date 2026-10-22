package web

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/sanitize"
	"github.com/motao123/rollcall/internal/store"
)

// 本文件实现公开状态页的管理接口：
// 展示配置、维护窗口、人工事件。
//
// 这些接口写的每一字节都会出现在匿名访客眼前，
// 因此校验比管理界面的其他接口更严：未知字段一律拒绝，
// 时间区间必须自洽，文本长度全部封顶。

// ---------- 展示配置 ----------

func (s *Server) handleAdminStatusPage(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.st.StatusPage()
		writeJSON(w, 200, map[string]any{
			"config":     cfg,
			"defaults":   model.DefaultStatusPage(),
			"nodeGroups": s.st.Groups(),
		})

	case http.MethodPut, http.MethodPost:
		body := http.MaxBytesReader(w, r.Body, 8<<10)
		buf, err := io.ReadAll(body)
		if err != nil {
			fail(w, 400, "读取请求体失败")
			return
		}
		// ★ map 字段（hiddenNodes / publicNotes）必须按"键是否出现"处理：
		// json.Unmarshal 解到已存在的 map 上是**合并**而不是替换，
		// 于是 {"publicNotes":{}} 清空不掉旧值 —— 生产验证时踩到过：
		// 界面里删掉备注、保存后备注仍在。
		var present map[string]json.RawMessage
		_ = json.Unmarshal(buf, &present)
		dec := json.NewDecoder(bytes.NewReader(buf))
		dec.DisallowUnknownFields()
		// ★ 必须从**当前已保存的配置**起解，而不是默认值。
		// 前端表单只提交 title/historyDays/announcement/visibility/blocks，
		// 从默认值起解会让未提交的 sections（模块顺序）被重置回默认顺序——
		// 表现就是"拖好顺序后点一次保存配置，顺序自己变回去了"。
		// 当前配置本身已是 DefaultStatusPage 合并过 JSON 的结果，
		// 所以对"新增字段"依然安全。
		cfg := s.st.StatusPage()
		if _, ok := present["publicNotes"]; ok {
			cfg.PublicNotes = nil
		}
		if _, ok := present["hiddenNodes"]; ok {
			cfg.HiddenNodes = nil
		}
		if err := dec.Decode(&cfg); err != nil {
			fail(w, 400, "bad json: "+err.Error())
			return
		}
		if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
			fail(w, 400, "请求体只能包含一个 JSON 对象")
			return
		}
		if cfg.Announcement.Enabled && !model.ValidLevel(cfg.Announcement.Level) {
			fail(w, 400, "公告级别必须是 info / warn / critical 之一")
			return
		}
		if msg := normalizeStatusGate(&cfg, s.st.Nodes()); msg != "" {
			fail(w, 400, msg)
			return
		}
		cfg.Announcement.UpdatedAt = time.Now().Unix()
		s.st.SetStatusPage(cfg)
		writeJSON(w, 200, map[string]any{"config": s.st.StatusPage()})

	default:
		fail(w, 405, "method not allowed")
	}
}

// randomToken 生成 URL 安全的随机串（base64url 无填充）。
// 用 crypto/rand 而不是时间派生：令牌会出现在浏览器历史与转发的链接里。
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// normalizeStatusGate 校验并收敛公开页的门槛与逐台配置。
//
// 这里的每一项都会影响匿名访客能看到什么，所以宁可拒绝也不"猜用户想要"：
// 未知节点 ID 直接报错，而不是静默丢弃 —— 静默丢弃会让人以为已经隐藏成功。
func normalizeStatusGate(cfg *model.StatusPage, nodes []*model.Node) string {
	if tok := strings.TrimSpace(cfg.Token); tok != "" {
		if len(tok) < 16 || len(tok) > 64 {
			return "访问令牌长度需在 16–64 个字符之间"
		}
		for _, c := range tok {
			ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
				(c >= '0' && c <= '9') || c == '-' || c == '_'
			if !ok {
				return "访问令牌只能包含字母、数字、连字符与下划线"
			}
		}
		cfg.Token = tok
	}

	known := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		known[n.ID] = true
	}

	if len(cfg.HiddenNodes) > 500 {
		return "隐藏节点最多 500 台"
	}
	seen := make(map[string]bool, len(cfg.HiddenNodes))
	hidden := make([]string, 0, len(cfg.HiddenNodes))
	for _, id := range cfg.HiddenNodes {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		if !known[id] {
			return "隐藏节点包含不存在的 ID: " + id
		}
		seen[id] = true
		hidden = append(hidden, id)
	}
	cfg.HiddenNodes = hidden

	if len(cfg.PublicNotes) > 500 {
		return "公开备注最多 500 条"
	}
	notes := make(map[string]string, len(cfg.PublicNotes))
	for id, text := range cfg.PublicNotes {
		id = strings.TrimSpace(id)
		if !known[id] {
			return "公开备注包含不存在的节点 ID: " + id
		}
		text = sanitize.Text(strings.TrimSpace(text), 120)
		if text == "" {
			continue
		}
		notes[id] = text
	}
	if len(notes) == 0 {
		notes = nil
	}
	cfg.PublicNotes = notes
	return ""
}

// handleAdminStatusToken 轮换公开页访问令牌。
//
// 独立成端点是为了"怀疑链接外泄时一键换链"：改配置要走整表单，
// 而换令牌必须能立刻生效、且旧链接立即失效。
func (s *Server) handleAdminStatusToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, 405, "method not allowed")
		return
	}
	cfg := s.st.StatusPage()
	tok, err := randomToken(24)
	if err != nil {
		fail(w, 500, "生成令牌失败")
		return
	}
	cfg.Token = tok
	s.st.SetStatusPage(cfg)
	writeJSON(w, 200, map[string]any{"token": tok, "enabled": cfg.IsEnabled()})
}

// ---------- 维护窗口 ----------

func (s *Server) handleAdminMaintenance(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		now := time.Now().Unix()
		writeJSON(w, 200, map[string]any{
			"items": s.st.Maintenances(),
			"now":   now,
		})

	case http.MethodPost:
		body := http.MaxBytesReader(w, r.Body, 8<<10)
		dec := json.NewDecoder(body)
		dec.DisallowUnknownFields()
		var req struct {
			ID      string   `json:"id"`
			Title   string   `json:"title"`
			Reason  string   `json:"reason"`
			NodeIDs []string `json:"nodeIds"`
			StartAt int64    `json:"startAt"`
			EndAt   int64    `json:"endAt"`
			Minutes int      `json:"minutes"` // 便捷入参：从开始时间起 N 分钟
		}
		if err := dec.Decode(&req); err != nil {
			fail(w, 400, "bad json: "+err.Error())
			return
		}
		if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
			fail(w, 400, "请求体只能包含一个 JSON 对象")
			return
		}
		now := time.Now().Unix()
		if req.StartAt <= 0 {
			req.StartAt = now
		}
		if req.EndAt <= 0 {
			// 未指定结束时间时按分钟数折算；两者都缺则默认 1 小时。
			mins := req.Minutes
			if mins <= 0 {
				mins = 60
			}
			if mins > 30*24*60 {
				fail(w, 400, "minutes 不能超过 43200（30 天）")
				return
			}
			req.EndAt = req.StartAt + int64(mins)*60
		}
		if req.EndAt <= req.StartAt {
			fail(w, 400, "结束时间必须晚于开始时间")
			return
		}
		if req.EndAt-req.StartAt > 30*24*3600 {
			fail(w, 400, "维护窗口不能超过 30 天")
			return
		}
		if req.ID != "" && !validRecordID(req.ID) {
			fail(w, 400, "维护窗口 ID 只能包含字母、数字、下划线与短横线")
			return
		}
		req.NodeIDs = dedupeStrings(trimListSpace(req.NodeIDs))
		for _, id := range req.NodeIDs {
			if _, ok := s.st.Node(id); !ok {
				fail(w, 400, "节点不存在: "+id)
				return
			}
		}
		m := &model.Maintenance{
			ID:        req.ID,
			Title:     req.Title,
			Reason:    req.Reason,
			NodeIDs:   req.NodeIDs,
			StartAt:   req.StartAt,
			EndAt:     req.EndAt,
			CreatedAt: now,
		}
		if m.ID == "" {
			m.ID = store.NewID("mt")
		}
		if err := s.st.UpsertMaintenance(m); err != nil {
			fail(w, 500, "保存维护窗口失败: "+err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"item": m})

	default:
		fail(w, 405, "method not allowed")
	}
}

func (s *Server) handleAdminMaintenanceByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/admin/maintenance/")
	if id == "" || strings.Contains(id, "/") {
		fail(w, 400, "invalid maintenance id")
		return
	}
	if r.Method != http.MethodDelete {
		fail(w, 405, "method not allowed")
		return
	}
	deleted, err := s.st.DeleteMaintenance(id)
	if err != nil {
		fail(w, 500, "删除维护窗口失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": deleted, "id": id})
}

// ---------- 人工事件 ----------

func (s *Server) handleAdminIncidents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, map[string]any{
			"items": s.st.Incidents(),
			"now":   time.Now().Unix(),
		})

	case http.MethodPost:
		body := http.MaxBytesReader(w, r.Body, 16<<10)
		dec := json.NewDecoder(body)
		dec.DisallowUnknownFields()
		var req struct {
			ID      string   `json:"id"`
			Title   string   `json:"title"`
			Body    string   `json:"body"`
			Level   string   `json:"level"`
			NodeIDs []string `json:"nodeIds"`
			StartAt int64    `json:"startAt"`
			EndAt   int64    `json:"endAt"`
		}
		if err := dec.Decode(&req); err != nil {
			fail(w, 400, "bad json: "+err.Error())
			return
		}
		if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
			fail(w, 400, "请求体只能包含一个 JSON 对象")
			return
		}
		if strings.TrimSpace(req.Title) == "" {
			fail(w, 400, "事件标题不能为空")
			return
		}
		// 先按存储层同样的规则清洗并回写：标题若只包含控制符/双向控制符，
		// 存储层会判定为空并返回错误，这里提前给出 400 而不是 500。
		req.Title = sanitize.Text(req.Title, 200)
		req.Body = sanitize.Text(req.Body, 4000)
		if req.Title == "" {
			fail(w, 400, "事件标题不能为空")
			return
		}
		if req.Level == "" {
			req.Level = model.LevelInfo
		}
		if !model.ValidLevel(req.Level) {
			fail(w, 400, "事件级别必须是 info / warn / critical 之一")
			return
		}
		if req.ID != "" && !validRecordID(req.ID) {
			fail(w, 400, "事件 ID 只能包含字母、数字、下划线与短横线")
			return
		}
		req.NodeIDs = dedupeStrings(trimListSpace(req.NodeIDs))
		for _, id := range req.NodeIDs {
			if _, ok := s.st.Node(id); !ok {
				fail(w, 400, "节点不存在: "+id)
				return
			}
		}
		now := time.Now().Unix()
		if req.StartAt <= 0 {
			req.StartAt = now
		}
		// EndAt == 0 表示仍在进行，是合法输入；其余情况下结束不能早于开始，
		// 不能像存储层那样静默改写成「进行中」。
		if req.EndAt != 0 && req.EndAt < req.StartAt {
			fail(w, 400, "事件结束时间不能早于开始时间")
			return
		}
		it := &model.Incident{
			ID:        req.ID,
			Title:     req.Title,
			Body:      req.Body,
			Level:     req.Level,
			NodeIDs:   req.NodeIDs,
			StartAt:   req.StartAt,
			EndAt:     req.EndAt,
			CreatedAt: now,
		}
		if it.ID == "" {
			it.ID = store.NewID("inc")
		}
		if err := s.st.UpsertIncident(it); err != nil {
			fail(w, 500, "保存事件失败: "+err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"item": it})

	default:
		fail(w, 405, "method not allowed")
	}
}

// validRecordID 校验管理员可回填的记录 ID。
// ID 会出现在 URL 路径与前端 DOM 中，限制为安全字符集，
// 避免斜杠、引号之类的内容破坏路由或模板。
func validRecordID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

func (s *Server) handleAdminIncidentByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/admin/incidents/")
	id, isUpdate := strings.CutSuffix(rest, "/updates")
	if id == "" || strings.Contains(id, "/") {
		fail(w, 400, "invalid incident id")
		return
	}

	switch r.Method {
	case http.MethodDelete:
		deleted, err := s.st.DeleteIncident(id)
		if err != nil {
			fail(w, 500, "删除事件失败: "+err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"deleted": deleted, "id": id})
		return
	case http.MethodPost:
		if !isUpdate {
			fail(w, 404, "unknown incident action")
			return
		}
	default:
		fail(w, 405, "method not allowed")
		return
	}

	body := http.MaxBytesReader(w, r.Body, 16<<10)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	var req struct {
		Status string `json:"status"`
		Text   string `json:"text"`
	}
	if err := dec.Decode(&req); err != nil {
		fail(w, 400, "bad json: "+err.Error())
		return
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		fail(w, 400, "请求体只能包含一个 JSON 对象")
		return
	}
	req.Text = sanitize.Text(req.Text, 500)
	if req.Text == "" {
		fail(w, 400, "更新内容不能为空")
		return
	}
	var cur *model.Incident
	for _, it := range s.st.Incidents() {
		if it.ID == id {
			cur = it
			break
		}
	}
	if cur == nil {
		fail(w, 404, "事件不存在")
		return
	}
	now := time.Now().Unix()
	status := model.NormalizeIncidentStatus(req.Status)
	cur.Updates = append(cur.Updates, model.IncidentUpdate{TS: now, Status: status, Text: req.Text})
	if status == model.IncidentResolved && cur.EndAt == 0 {
		cur.EndAt = now
	}
	if err := s.st.UpsertIncident(cur); err != nil {
		fail(w, 500, "保存事件更新失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"item": cur})
}

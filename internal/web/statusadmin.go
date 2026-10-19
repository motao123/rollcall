package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/motao/rollcall/internal/model"
	"github.com/motao/rollcall/internal/store"
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
		dec := json.NewDecoder(body)
		dec.DisallowUnknownFields()
		// 从默认值起解：前端只传它关心的字段时，
		// 其余项保持默认而不是被读成 false 把页面关空。
		cfg := model.DefaultStatusPage()
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
		cfg.Announcement.UpdatedAt = time.Now().Unix()
		s.st.SetStatusPage(cfg)
		writeJSON(w, 200, map[string]any{"config": s.st.StatusPage()})

	default:
		fail(w, 405, "method not allowed")
	}
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
			Minutes int      `json:"minutes"` // 便捷入参：从现在起 N 分钟
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
		req.NodeIDs = trimListSpace(req.NodeIDs)
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
		s.st.UpsertMaintenance(m)
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
	writeJSON(w, 200, map[string]any{"deleted": s.st.DeleteMaintenance(id), "id": id})
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
		if req.Level == "" {
			req.Level = model.LevelInfo
		}
		if !model.ValidLevel(req.Level) {
			fail(w, 400, "事件级别必须是 info / warn / critical 之一")
			return
		}
		req.NodeIDs = trimListSpace(req.NodeIDs)
		for _, id := range req.NodeIDs {
			if _, ok := s.st.Node(id); !ok {
				fail(w, 400, "节点不存在: "+id)
				return
			}
		}
		now := time.Now().Unix()
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
		s.st.UpsertIncident(it)
		writeJSON(w, 200, map[string]any{"item": it})

	default:
		fail(w, 405, "method not allowed")
	}
}

func (s *Server) handleAdminIncidentByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/admin/incidents/")
	if id == "" || strings.Contains(id, "/") {
		fail(w, 400, "invalid incident id")
		return
	}
	if r.Method != http.MethodDelete {
		fail(w, 405, "method not allowed")
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": s.st.DeleteIncident(id), "id": id})
}

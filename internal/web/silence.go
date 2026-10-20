package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/store"
)

// ---------- 通知队列管理 ----------

func (s *Server) handleAdminNotifyJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, 405, "method not allowed")
		return
	}
	writeJSON(w, 200, map[string]any{
		"stats": s.st.NotifyStats(),
		"jobs":  s.st.NotifyJobs(100),
	})
}

func (s *Server) handleAdminNotifyJobByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/admin/notify/jobs/")
	if !strings.HasSuffix(rest, "/retry") {
		fail(w, 404, "not found")
		return
	}
	id := strings.TrimSuffix(rest, "/retry")
	if id == "" || strings.Contains(id, "/") {
		fail(w, 404, "not found")
		return
	}
	if r.Method != http.MethodPost {
		fail(w, 405, "method not allowed")
		return
	}
	err := s.st.RetryNotifyJob(id, time.Now().Unix())
	switch {
	case err == nil:
		if s.outbox != nil {
			s.outbox.Wake()
		}
		writeJSON(w, 200, map[string]any{"ok": true, "id": id})
	case errors.Is(err, store.ErrNotifyJobNotFound):
		fail(w, 404, "通知任务不存在")
	case errors.Is(err, store.ErrNotifyJobPending):
		fail(w, 409, "通知任务已在队列中")
	case errors.Is(err, store.ErrNotifyJobSent):
		fail(w, 409, "通知任务已投递成功，无需重试")
	default:
		fail(w, 500, "重试通知任务失败")
	}
}

// ---------- 通知静默 ----------
//
// 静默只抑制通知投递，告警记录与恢复流程不受影响。
// scope 为 "global" 或规则 ID。

func (s *Server) handleAdminSilences(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, 405, "method not allowed")
		return
	}
	now := time.Now().Unix()
	writeJSON(w, 200, map[string]any{
		"items": s.st.Silences(now),
		"now":   now,
	})
}

func (s *Server) handleAdminSilenceByScope(w http.ResponseWriter, r *http.Request) {
	scope := strings.TrimPrefix(r.URL.Path, "/api/admin/silences/")
	if scope == "" || strings.Contains(scope, "/") {
		fail(w, 400, "invalid silence scope")
		return
	}

	switch r.Method {
	case http.MethodPut, http.MethodPost:
		// 规则存在性只影响「新建静默」：规则删除后遗留的静默仍应可删除，
		// 否则 GET 还能看到它、DELETE 却永远 404。
		if scope != "global" {
			if _, ok := s.ruleByID(scope); !ok {
				fail(w, 404, "rule not found")
				return
			}
		}
		body := http.MaxBytesReader(w, r.Body, 4<<10)
		var req struct {
			Minutes int    `json:"minutes"`
			Reason  string `json:"reason"`
		}
		dec := json.NewDecoder(body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			fail(w, 400, "bad json: "+err.Error())
			return
		}
		// json.Decoder 只消费第一个值，`{"minutes":60}{"minutes":43200}`
		// 这种请求体会被静默当成前者。尾随内容一律拒绝。
		if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
			fail(w, 400, "请求体只能包含一个 JSON 对象")
			return
		}
		if req.Minutes < 1 || req.Minutes > 30*24*60 {
			fail(w, 400, "minutes 必须在 1–43200 之间")
			return
		}
		reason := strings.TrimSpace(req.Reason)
		if runes := []rune(reason); len(runes) > 200 {
			reason = string(runes[:200])
		}
		now := time.Now().Unix()
		sl := &model.Silence{
			Scope:     scope,
			Until:     now + int64(req.Minutes)*60,
			Reason:    reason,
			CreatedAt: now,
		}
		s.st.SetSilence(sl)
		writeJSON(w, 200, map[string]any{"silence": sl, "now": now})

	case http.MethodDelete:
		removed := s.st.DeleteSilence(scope)
		writeJSON(w, 200, map[string]any{"deleted": removed, "scope": scope})

	default:
		fail(w, 405, "method not allowed")
	}
}

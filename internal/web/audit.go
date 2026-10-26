package web

import (
	"crypto/subtle"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/store"
)

// handleAdminAudit 查询管理操作审计日志。
//
//	GET /api/admin/audit?limit=50&offset=0&actor=&action=&q=&range=7d
func (s *Server) handleAdminAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	aq := store.AuditQuery{
		Limit:  limit,
		Offset: offset,
		Actor:  q.Get("actor"),
		Action: q.Get("action"),
		Search: q.Get("q"),
	}
	now := time.Now().Unix()
	switch q.Get("range") {
	case "24h":
		aq.Since = now - 86400
	case "7d":
		aq.Since = now - 7*86400
	case "30d":
		aq.Since = now - 30*86400
	case "90d":
		aq.Since = now - 90*86400
	}
	if v, err := strconv.ParseInt(q.Get("since"), 10, 64); err == nil && v > 0 {
		aq.Since = v
	}
	if v, err := strconv.ParseInt(q.Get("until"), 10, 64); err == nil && v > 0 {
		aq.Until = v
	}
	entries, total := s.st.QueryAudit(aq)
	if entries == nil {
		entries = []model.AuditEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": entries,
		"total":   total,
		"limit":   limit,
		"offset":  offset,
		"stats":   s.st.AuditStats(),
	})
}

// auditActor 从请求中解析审计主体。按鉴权优先级判断，
// 与 requireAdmin 的检查顺序保持一致。
func (s *Server) auditActor(r *http.Request) (actor, role string) {
	if s.adminToken != "" {
		if t := r.Header.Get("X-Admin-Token"); t != "" &&
			subtle.ConstantTimeCompare([]byte(t), []byte(s.adminToken)) == 1 {
			return "admin-token", "admin-token"
		}
		if _, pw, ok := r.BasicAuth(); ok &&
			subtle.ConstantTimeCompare([]byte(pw), []byte(s.adminToken)) == 1 {
			return "admin-token", "admin-token"
		}
	}
	if s.panelBasicOK(r) {
		return s.authUser, "basic"
	}
	if t, ok := s.matchAPIToken(r); ok {
		return t.Name, "api-token"
	}
	if _, u, ok := s.sessionUserWithUser(r); ok {
		return u.Username, "session"
	}
	return "unknown", ""
}

// auditAdminMutation 在鉴权中间件之后记录管理写操作。
// 只记录元数据：路径不含查询串，绝不读取请求体（可能有密码/令牌）。
func (s *Server) auditAdminMutation(r *http.Request, status int) {
	p := r.URL.Path
	if !strings.HasPrefix(p, "/api/admin/") && !strings.HasPrefix(p, "/api/export/") {
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return
	}
	actor, role := s.auditActor(r)
	s.st.AddAudit(model.AuditEntry{
		Action: auditAction(r.Method, p),
		Actor:  actor,
		Role:   role,
		IP:     clientIP(r, s.trustedProxies),
		Method: r.Method,
		Path:   p,
		Status: status,
	})
}

func auditName(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "unknown"
	}
	if r := []rune(v); len(r) > 64 {
		return string(r[:64])
	}
	return v
}

func (s *Server) auditAuth(r *http.Request, action, actor, role string, status int, detail string) {
	s.st.AddAudit(model.AuditEntry{
		Action: action,
		Actor:  actor,
		Role:   role,
		IP:     clientIP(r, s.trustedProxies),
		Method: r.Method,
		Path:   r.URL.Path,
		Status: status,
		Detail: detail,
	})
}

// auditAction 把管理 API 路径映射成稳定、可筛选的动作名。
func auditAction(method, p string) string {
	resource := "admin"
	switch {
	case strings.HasPrefix(p, "/api/export/"):
		resource = "export"
	case p == "/api/admin/nodes/batch":
		return "nodes.batch"
	case p == "/api/admin/nodes/import":
		return "nodes.import"
	case strings.HasPrefix(p, "/api/admin/nodes"):
		resource = "nodes"
	case strings.HasPrefix(p, "/api/admin/agent-upgrades"):
		resource = "agent-upgrades"
	case strings.HasPrefix(p, "/api/admin/services"):
		resource = "services"
	case strings.HasPrefix(p, "/api/admin/settings"):
		resource = "settings"
	case strings.HasPrefix(p, "/api/admin/account"):
		resource = "account"
	case strings.HasPrefix(p, "/api/admin/tokens"):
		resource = "tokens"
	case p == "/api/admin/backup":
		return "backup.export"
	case strings.HasPrefix(p, "/api/admin/backup/auto/run"):
		return "backup.run"
	case strings.HasPrefix(p, "/api/admin/backup"):
		resource = "backup"
	case strings.HasPrefix(p, "/api/admin/restore"):
		return "backup.restore"
	case strings.HasPrefix(p, "/api/admin/theme"):
		resource = "theme"
	case strings.HasPrefix(p, "/api/admin/test-notify"):
		return "notify.test"
	case strings.HasPrefix(p, "/api/admin/rules"):
		resource = "rules"
	case strings.HasPrefix(p, "/api/admin/rule-meta"):
		resource = "rules"
	case strings.HasPrefix(p, "/api/admin/silences"):
		resource = "silences"
	case strings.HasPrefix(p, "/api/admin/status-page"):
		resource = "statuspage"
	case strings.HasPrefix(p, "/api/admin/maintenance"):
		resource = "maintenance"
	case strings.HasSuffix(p, "/updates") && strings.HasPrefix(p, "/api/admin/incidents"):
		return "incidents.update"
	case strings.HasPrefix(p, "/api/admin/incidents"):
		resource = "incidents"
	case strings.HasPrefix(p, "/api/admin/notify"):
		resource = "notify"
	case strings.HasPrefix(p, "/api/admin/renewals"):
		resource = "renewals"
	}
	verb := "action"
	switch method {
	case http.MethodPost:
		// 集合端点上的 POST 是创建；子资源上的 POST 是动作（如结束维护）。
		if strings.Count(strings.TrimSuffix(path.Clean(p), "/"), "/") <= 3 {
			verb = "create"
		}
	case http.MethodPut, http.MethodPatch:
		verb = "update"
	case http.MethodDelete:
		verb = "delete"
	}
	return resource + "." + verb
}

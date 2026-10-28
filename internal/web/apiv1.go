package web

import (
	"net/http"
	"strconv"
	"time"

	"github.com/motao123/rollcall/internal/store"
)

// 本文件提供 v1 分页接口：把历史上"只返回最近 N 条"的三个列表
// （告警历史 / 通知投递队列 / 管理审计）改为服务端分页 + 统一响应信封。
//
// 为什么新增 /api/v1/ 而不是直接改老路由：
//   - 老路由的响应形状（entries/items/total/stats）已被面板前端、README 示例
//     和外部脚本依赖，改形状属于破坏性变更；
//   - 老路由的语义是"取最近 N 条"，没有 offset 语义，强行加 offset 会让
//     "最近 N 条"与"第 k 页"两套语义纠缠在同一条路径上。
//
// 因此：老路由原样保留（兼容），新路由提供分页与统一信封（推荐）。
// 两者的数据源与排序完全一致，不会出现"新旧页面看到的历史不同"。

const (
	// v1DefaultLimit 是未显式指定 limit 时的每页条数。
	v1DefaultLimit = 20
	// v1MaxLimit 是单页条数硬上限。超过上限按上限截断而不是报错 ——
	// 分页接口的意义是限制单次响应体积，报错只会把调用方卡死。
	v1MaxLimit = 200
)

// pageParams 解析 limit/offset 并夹取到合法区间。
//
// limit<=0 视为未指定（用默认值）；offset<0 归零。
// 返回值即"实际生效值"，写进响应信封，调用方据此判断是否需要翻页。
func pageParams(r *http.Request, defLimit, maxLimit int) (limit, offset int) {
	q := r.URL.Query()
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil || limit <= 0 {
		limit = defLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	offset, err = strconv.Atoi(q.Get("offset"))
	if err != nil || offset < 0 {
		offset = 0
	}
	return limit, offset
}

// writePage 输出统一分页信封：
//
//	{ "items": [...], "total": N, "limit": L, "offset": O, "hasMore": bool, ...extra }
//
// total 是**筛选后的匹配总数**（不是当前页条数），前端据此算总页数；
// hasMore 是 offset+len(items) < total 的等价判断，客户端可以只靠它做「下一页」
// 而不必自己算（两者同时给出是为了兼容两种调用习惯）。
//
// extra 直接并到顶层，用于携带该接口的附属字段（如 active/stats），
// 这样前端一次请求即可渲染整页，不必再打一次老路由。
func writePage[T any](w http.ResponseWriter, items []T, total, limit, offset int, extra map[string]any) {
	if items == nil {
		items = []T{}
	}
	if limit <= 0 {
		limit = v1DefaultLimit
	}
	body := make(map[string]any, 6+len(extra))
	for k, v := range extra {
		body[k] = v
	}
	body["items"] = items
	body["total"] = total
	body["limit"] = limit
	body["offset"] = offset
	body["hasMore"] = offset+len(items) < total
	writeJSON(w, http.StatusOK, body)
}

// handleV1Alerts 分页查询告警历史。
//
//	GET /api/v1/alerts?limit=20&offset=0&state=all|active|resolved&q=关键词
//
// 附带的 active/notify/silences 与 /api/alerts 完全一致，前端可一次拿全。
func (s *Server) handleV1Alerts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	limit, offset := pageParams(r, v1DefaultLimit, v1MaxLimit)
	q := r.URL.Query()
	items, total := s.st.AlertsPage(store.AlertPageQuery{
		Limit:  limit,
		Offset: offset,
		State:  q.Get("state"),
		Search: q.Get("q"),
	})
	now := time.Now().Unix()
	writePage(w, items, total, limit, offset, map[string]any{
		"active":   s.st.ActiveAlertCount(),
		"silences": s.st.Silences(now),
		"notify":   s.st.NotifyStats(),
		"now":      now,
	})
}

// handleV1NotifyJobs 分页查询通知投递队列。
//
//	GET /api/v1/admin/notify/jobs?limit=20&offset=0&status=pending|sent|failed|suppressed
func (s *Server) handleV1NotifyJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	limit, offset := pageParams(r, v1DefaultLimit, v1MaxLimit)
	items, total := s.st.NotifyJobsPage(store.NotifyJobPageQuery{
		Limit:  limit,
		Offset: offset,
		Status: r.URL.Query().Get("status"),
	})
	writePage(w, items, total, limit, offset, map[string]any{
		"stats": s.st.NotifyStats(),
	})
}

// handleV1Audit 分页查询管理审计日志（与 /api/admin/audit 同源同序）。
//
//	GET /api/v1/admin/audit?limit=20&offset=0&range=7d&actor=&action=&q=
func (s *Server) handleV1Audit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// 上限与 store 的审计查询上限对齐：若这里放宽，存储层会静默截断，
	// 响应里报告的 limit 就与实际返回条数不符。
	limit, offset := pageParams(r, v1DefaultLimit, store.AuditQueryMaxLimit)
	aq := auditQueryFromRequest(r, limit, offset)
	entries, total := s.st.QueryAudit(aq)
	writePage(w, entries, total, limit, offset, map[string]any{
		"stats": s.st.AuditStats(),
	})
}

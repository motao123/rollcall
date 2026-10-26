package web

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/motao123/rollcall/internal/model"
)

// 令牌读访问审计。
//
// 管理写操作本来就有审计（auditAdminMutation），但**读**一直被刻意跳过 ——
// 面板自己每几秒轮询一次，逐条记录会把审计文件刷爆。代价是：用作用域令牌
// 读数据这件事完全不可见，而 `rollcall-mcp` 正是"用令牌调只读 API"的形态，
// 于是「MCP 被用来查了什么、从哪台机器查的」无从回答。
//
// 折中办法：只记**令牌**发起的读请求（面板自身的会话不记），并且同一令牌
// 在同一路由上按窗口合并 —— 一次 MCP 会话里反复调用同一个工具只会留一条。
const (
	// tokenAuditWindowSec 是同一（令牌, 路由）组合的记录间隔。
	// 取 5 分钟：足够回答"谁在什么时候读过什么"，又不会被脚本轮询刷爆。
	tokenAuditWindowSec = 300
	// tokenAuditMaxKeys 限制去重表大小，防止大量令牌 × 路由把内存吃掉。
	tokenAuditMaxKeys = 1024
)

// tokenReadAuditor 记住每个（令牌, 路由）上次记录的时刻。
type tokenReadAuditor struct {
	seen map[string]int64
}

func newTokenReadAuditor() *tokenReadAuditor {
	return &tokenReadAuditor{seen: make(map[string]int64)}
}

// allow 判断这次是否应当落一条审计。
func (a *tokenReadAuditor) allow(key string, now int64) bool {
	if a == nil {
		return false
	}
	if prev, ok := a.seen[key]; ok && now-prev < tokenAuditWindowSec {
		return false
	}
	if len(a.seen) >= tokenAuditMaxKeys {
		// 满了先清过期项；仍然满就整体重置 —— 少几条审计好过无界增长。
		for k, ts := range a.seen {
			if now-ts >= tokenAuditWindowSec {
				delete(a.seen, k)
			}
		}
		if len(a.seen) >= tokenAuditMaxKeys {
			a.seen = make(map[string]int64)
		}
	}
	a.seen[key] = now
	return true
}

// auditTokenRead 在请求结束后记录令牌发起的读访问。
//
// 只处理 GET/HEAD + Bearer rct_ 令牌：
//   - 写操作已由 auditAdminMutation 记录，这里再来一条会重复；
//   - 会话（浏览器）访问不记：那是人自己点面板，不是自动化。
func (s *Server) auditTokenRead(r *http.Request, route string, status int, dur time.Duration) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return
	}
	// 先做字符串判断，避免对每个普通请求都去比对令牌摘要。
	h := r.Header.Get("Authorization")
	if len(h) < 12 || !strings.EqualFold(h[:7], "Bearer ") ||
		!strings.HasPrefix(strings.TrimSpace(h[7:]), APITokenPrefix) {
		return
	}
	// SSE 是长连接，连接存活时间没有意义；而且它本来也不会被 MCP 使用。
	if isStreamRoute(route) {
		return
	}
	t, ok := s.matchAPIToken(r)
	if !ok {
		return
	}
	now := time.Now().Unix()
	key := t.ID + "|" + r.Method + "|" + route
	if !s.tokenAudit.allow(key, now) {
		return
	}
	s.st.AddAudit(model.AuditEntry{
		Action: "api.read",
		Actor:  auditName(t.Name),
		Role:   "api-token",
		IP:     clientIP(r, s.trustedProxies),
		Method: r.Method,
		// 不含查询串：公开页的 ?t= 就是访问令牌，日志里不能留副本。
		Path:   r.URL.Path,
		Status: status,
		Detail: fmt.Sprintf("%dms（同令牌同路径 %.0f 分钟内只记一条）", dur.Milliseconds(), tokenAuditWindowSec/60.0),
	})
}

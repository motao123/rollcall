// Package web 提供 HTTP API 与内嵌前端资源。
package web

import (
	"compress/gzip"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/motao/rollcall/internal/alert"
	"github.com/motao/rollcall/internal/model"
	"github.com/motao/rollcall/internal/notify"
	"github.com/motao/rollcall/internal/outbox"
	"github.com/motao/rollcall/internal/probe"
	"github.com/motao/rollcall/internal/store"
)

//go:embed dist
var assets embed.FS

// Server 汇总所有依赖。
type Server struct {
	st         *store.Store
	runner     *probe.Runner
	engine     *alert.Engine
	adminToken string
	authUser   string
	authPass   string
	panelLink  string
	cnyRate    float64
	limiter    *authFailureLimiter
	outbox     *outbox.Manager
	notifyErr  error

	// trustedProxies 是允许提交 X-Forwarded-For 的反向代理网段。
	// 为空时只信任回环（见 defaultTrustedProxies）。
	trustedProxies []*net.IPNet

	// agentDir 是 Agent 二进制分发目录（-agent-dir）。
	// 为空表示不提供「一条命令接入」，下载接口会明确报错。
	agentDir string

	testMu   sync.Mutex
	lastTest time.Time

	// notifySaveMu 串行化「写设置 + 应用配置」这一对操作。
	// 两者是分开的两步：两个并发保存可能交错成「持久化的是 A、内存生效的是 B」，
	// 重启后行为突然改变。
	notifySaveMu sync.Mutex
}

// Config 是服务端的运行参数。
type Config struct {
	AdminToken string // 管理 API 令牌（为空则不开放管理 API）
	AuthUser   string // Basic Auth 用户名（为空则不启用）
	AuthPass   string
	PanelLink  string          // 面板对外地址，拼进通知正文
	CNYRate    float64         // CNY→USD 折算汇率，<=0 时使用 7.2
	Outbox     *outbox.Manager // 通知投递队列；nil 时不外发通知
	AgentDir   string          // Agent 二进制分发目录；空则不提供下载
	// TrustedProxies 是可信反向代理网段；nil 时仅信任回环。
	// 跨机代理必须显式配置，否则无法识别真实客户端 IP。
	TrustedProxies []*net.IPNet
}

func New(st *store.Store, r *probe.Runner, e *alert.Engine, cfg Config) *Server {
	rate := cfg.CNYRate
	if rate <= 0 {
		rate = 7.2
	}
	trusted := cfg.TrustedProxies
	if trusted == nil {
		trusted = defaultTrustedProxies
	}
	srv := &Server{st: st, runner: r, engine: e,
		adminToken: cfg.AdminToken, authUser: cfg.AuthUser, authPass: cfg.AuthPass,
		panelLink: cfg.PanelLink, cnyRate: rate,
		limiter: newAuthFailureLimiter(), outbox: cfg.Outbox,
		agentDir:       cfg.AgentDir,
		trustedProxies: trusted}
	// 载入已保存的通知配置并挂到告警引擎
	if nc, err := notify.Parse(st.GetSetting("notify")); err != nil {
		srv.notifyErr = err
	} else {
		srv.applyNotifyConfig(nc)
	}
	return srv
}

// NotifyErr 返回启动时通知配置的解析错误，供 main 记录日志。
func (s *Server) NotifyErr() error { return s.notifyErr }

// applyNotifyConfig 把通知配置同步到投递队列与告警引擎。
// 引擎回调只做入队；真正的 HTTP 投递由 Outbox 的有界 worker 完成。
func (s *Server) applyNotifyConfig(cfg *notify.Config) {
	if s.outbox != nil {
		s.outbox.SetConfig(cfg)
	}
	if s.engine == nil {
		return
	}
	s.engine.SetRepeat(cfg.RepeatInterval)
	s.engine.SetLink(s.panelLink)
	if s.outbox != nil {
		s.engine.SetNotifier(s.outbox.Enqueue)
		return
	}
	s.engine.SetNotifier(nil)
}

// authLevel 声明一条路由的鉴权边界。
//
// 路由表是唯一的鉴权来源：所有接口都必须在 routes 中登记级别，
// 漏填（零值）或填写未知级别会让 Handler 在启动时 panic，
// 避免"新增接口却忘了鉴权"。
type authLevel int

const (
	authUnclassified authLevel = iota // 零值：故意非法，用于启动校验
	authPublic                        // 完全公开（健康检查、公开状态页）
	authNodeToken                     // handler 内校验 X-Node-Token（Agent 上报）
	authPanel                         // 启用 -auth 时要求面板 Basic 凭据
	authReadonly                      // 管理令牌，或已登录面板用户（只读仪表盘）
	authAdmin                         // 仅 X-Admin-Token（管理写操作、CSV 导出）
	authMetrics                       // 管理令牌或面板凭据（Prometheus 抓取）
)

func (l authLevel) valid() bool { return l > authUnclassified && l <= authMetrics }

func (l authLevel) String() string {
	switch l {
	case authPublic:
		return "public"
	case authNodeToken:
		return "node-token"
	case authPanel:
		return "panel"
	case authReadonly:
		return "readonly"
	case authAdmin:
		return "admin"
	case authMetrics:
		return "metrics"
	default:
		return "unclassified"
	}
}

// protected 表示该级别下 2xx 响应能否证明请求持有有效凭证，
// 用于清零鉴权失败限速计数。公开页与 Agent 上报不能清零。
func (l authLevel) protected(authConfigured bool) bool {
	switch l {
	case authReadonly, authAdmin, authMetrics:
		return true
	case authPanel:
		return authConfigured
	default:
		return false
	}
}

// route 是路由表的一项。
type route struct {
	pattern string
	level   authLevel
	handler http.Handler
}

// routes 返回全部路由及其鉴权级别。
//
// 静态资源挂在 "/" 上，启用 -auth 时整站要求 Basic；
// 公开状态页只依赖 /status.js，因此必须单独提前登记为公开，
// 否则 ServeMux 会把它落到受保护的 "/" 上，页面卡在骨架屏。
func (s *Server) routes(static http.Handler) []route {
	return []route{
		// 公开端点
		{"/healthz", authPublic, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "ok")
		})},
		{"/status", authPublic, http.HandlerFunc(s.handleStatusPage)},
		{"/status.js", authPublic, static},
		{"/api/status", authPublic, http.HandlerFunc(s.handleStatus)},
		// 一条命令接入：脚本与二进制都公开（脚本不含密钥，token 由调用方传参）
		{"/agent/install.sh", authPublic, http.HandlerFunc(s.handleAgentInstall)},
		{"/agent/download", authPublic, http.HandlerFunc(s.handleAgentDownload)},
		{"/api/theme", authPublic, http.HandlerFunc(s.handleTheme)},

		// Agent 上报：handler 内校验 X-Node-Token
		{"/api/report", authNodeToken, http.HandlerFunc(s.handleReport)},

		// 仪表盘只读数据包含主机名、内核、服务商、价格等资产信息，
		// 不能像公开状态页那样匿名下发。
		{"/api/overview", authReadonly, http.HandlerFunc(s.handleOverview)},
		{"/api/nodes", authReadonly, http.HandlerFunc(s.handleNodes)},
		{"/api/nodes/", authReadonly, http.HandlerFunc(s.handleNodeDetail)},
		{"/api/services", authReadonly, http.HandlerFunc(s.handleServices)},
		{"/api/alerts", authReadonly, http.HandlerFunc(s.handleAlerts)},
		{"/api/rules", authReadonly, http.HandlerFunc(s.handleRules)},
		{"/api/assets", authReadonly, http.HandlerFunc(s.handleAssets)},

		// 管理写操作与运营数据导出：仅 X-Admin-Token
		{"/api/admin/nodes", authAdmin, http.HandlerFunc(s.handleAdminNodes)},
		{"/api/admin/nodes/", authAdmin, http.HandlerFunc(s.handleAdminNodeByID)},
		{"/api/admin/services", authAdmin, http.HandlerFunc(s.handleAdminServices)},
		{"/api/admin/services/", authAdmin, http.HandlerFunc(s.handleAdminServiceByID)},
		{"/api/admin/settings", authAdmin, http.HandlerFunc(s.handleAdminSettings)},
		{"/api/admin/theme", authAdmin, http.HandlerFunc(s.handleAdminTheme)},
		{"/api/admin/test-notify", authAdmin, http.HandlerFunc(s.handleTestNotify)},
		{"/api/admin/rules", authAdmin, http.HandlerFunc(s.handleAdminRules)},
		{"/api/admin/rules/", authAdmin, http.HandlerFunc(s.handleAdminRuleByID)},
		{"/api/admin/rule-meta", authAdmin, http.HandlerFunc(s.handleRuleMeta)},
		{"/api/admin/silences", authAdmin, http.HandlerFunc(s.handleAdminSilences)},
		{"/api/admin/silences/", authAdmin, http.HandlerFunc(s.handleAdminSilenceByScope)},
		{"/api/admin/notify/jobs", authAdmin, http.HandlerFunc(s.handleAdminNotifyJobs)},
		{"/api/admin/notify/jobs/", authAdmin, http.HandlerFunc(s.handleAdminNotifyJobByID)},
		{"/api/export/", authAdmin, http.HandlerFunc(s.handleExport)},
		{"/metrics", authMetrics, http.HandlerFunc(s.handleMetrics)},

		// 面板静态资源
		{"/", authPanel, static},
	}
}

// validateRoutes 校验路由表并返回 pattern → 级别索引。
// 任何未分类、重复或缺 handler 的登记项都会让进程启动失败。
func validateRoutes(routes []route) (map[string]authLevel, error) {
	levels := make(map[string]authLevel, len(routes))
	for _, rt := range routes {
		if rt.pattern == "" {
			return nil, fmt.Errorf("route with empty pattern")
		}
		if !strings.HasPrefix(rt.pattern, "/") {
			return nil, fmt.Errorf("route %q must start with /", rt.pattern)
		}
		if rt.handler == nil {
			return nil, fmt.Errorf("route %q has no handler", rt.pattern)
		}
		if !rt.level.valid() {
			return nil, fmt.Errorf("route %q has no valid auth level (got %s)", rt.pattern, rt.level)
		}
		if _, dup := levels[rt.pattern]; dup {
			return nil, fmt.Errorf("duplicate route %q", rt.pattern)
		}
		levels[rt.pattern] = rt.level
	}
	if _, ok := levels["/"]; !ok {
		return nil, fmt.Errorf(`missing "/" catch-all route`)
	}
	return levels, nil
}

// Handler 返回配置好的路由。
func (s *Server) Handler() http.Handler {
	// 用户名已配置但密码为空属于配置错误，必须整体拒绝而不是放行空密码
	if s.authUser != "" && s.authPass == "" {
		return withCommon(withGzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "server auth misconfigured", http.StatusInternalServerError)
		})))
	}

	sub, err := fs.Sub(assets, "dist")
	if err != nil {
		panic(err)
	}
	static := http.FileServer(http.FS(sub))

	routes := s.routes(static)
	levels, err := validateRoutes(routes)
	if err != nil {
		panic("rollcall: invalid route table: " + err.Error())
	}

	mux := http.NewServeMux()
	for _, rt := range routes {
		mux.Handle(rt.pattern, s.guard(rt.level, rt.handler))
	}

	// 用 ServeMux 自己的匹配结果反查鉴权级别，避免再维护一份路径白名单。
	levelOf := func(r *http.Request) authLevel {
		_, pattern := mux.Handler(r)
		if l, ok := levels[pattern]; ok {
			return l
		}
		// 仅路径清理重定向等无 pattern 的情况会到这里；
		// 按非受保护处理，避免 3xx 错误地清零爆破计数。
		return authPublic
	}

	return withCommon(withGzip(s.withAuthGuard(mux, levelOf)))
}

// guard 按路由表声明的级别包装 handler。
func (s *Server) guard(level authLevel, h http.Handler) http.Handler {
	switch level {
	case authPublic, authNodeToken:
		return h
	case authPanel:
		return s.requirePanel(h)
	case authReadonly:
		return s.requireReadonly(h)
	case authAdmin:
		return s.requireAdminToken(h)
	case authMetrics:
		return s.requireMetrics(h)
	default:
		// validateRoutes 已拦截；这里保留兜底，绝不静默放行
		panic("rollcall: unclassified route")
	}
}

// requirePanel 对面板静态资源启用 Basic Auth；未配置 -auth 时直接放行。
func (s *Server) requirePanel(h http.Handler) http.Handler {
	if s.authUser == "" {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.panelBasicOK(r) {
			h.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="Rollcall", charset="UTF-8"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// requireReadonly 保护仪表盘只读接口。
//
// 管理令牌优先；启用 -auth 时也接受面板 Basic 凭据，避免已登录用户
// 为了查看只读页面还要再粘贴一次令牌。写操作仍只认 X-Admin-Token。
func (s *Server) requireReadonly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.adminTokenOK(r) || s.panelBasicOK(r) {
			h.ServeHTTP(w, r)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// requireAdminToken 保护管理写操作与导出，只认 X-Admin-Token。
func (s *Server) requireAdminToken(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.adminOK(r) {
			h.ServeHTTP(w, r)
			return
		}
		fail(w, http.StatusUnauthorized, "invalid admin token")
	})
}

// requireMetrics 保护 Prometheus 端点，401 时返回 Basic 挑战方便抓取端配置。
func (s *Server) requireMetrics(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.metricsAuthOK(r) {
			h.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="Rollcall metrics"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// adminTokenOK 接受 X-Admin-Token，或 Basic 密码为管理令牌（用户名任意）。
func (s *Server) adminTokenOK(r *http.Request) bool {
	if s.adminToken == "" {
		return false
	}
	if t := r.Header.Get("X-Admin-Token"); t != "" {
		return subtle.ConstantTimeCompare([]byte(t), []byte(s.adminToken)) == 1
	}
	if _, pw, ok := r.BasicAuth(); ok {
		return subtle.ConstantTimeCompare([]byte(pw), []byte(s.adminToken)) == 1
	}
	return false
}

// panelBasicOK 校验收到的 Basic Auth 是否匹配 -auth 配置的面板凭据。
func (s *Server) panelBasicOK(r *http.Request) bool {
	if s.authUser == "" || s.authPass == "" {
		return false
	}
	u, p, ok := r.BasicAuth()
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(u), []byte(s.authUser)) == 1 &&
		subtle.ConstantTimeCompare([]byte(p), []byte(s.authPass)) == 1
}

// withGzip 对 /api/ 下的 JSON/CSV 响应做透明压缩。
// 365 天曲线可达 2MB 以上，经 SSH 隧道或慢速链路传输时浏览器会长时间挂起；
// 压缩后通常降到几百 KB，且客户端不声明 gzip 时行为完全不变。
func withGzip(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			h.ServeHTTP(w, r)
			return
		}
		w.Header().Add("Vary", "Accept-Encoding")
		if !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			h.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w}
		defer func() { _ = gw.Close() }()
		h.ServeHTTP(gw, r)
	})
}

// acceptsGzip 解析 Accept-Encoding，显式 q=0 视为拒绝。
func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		fields := strings.Split(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(fields[0]), "gzip") &&
			!strings.EqualFold(strings.TrimSpace(fields[0]), "x-gzip") {
			continue
		}
		for _, p := range fields[1:] {
			p = strings.TrimSpace(p)
			// 只比较字面量 "0" 会漏掉 0.0 / 0.00 / 0.000 这几种等价写法，
			// 而它们明确表示「不接受 gzip」。
			if v, ok := strings.CutPrefix(p, "q="); ok {
				if q, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && q <= 0 {
					return false
				}
			}
		}
		return true
	}
	return false
}

// gzipResponseWriter 延迟到内容类型确定后再决定是否压缩：
// http.Error 的 text/plain、304 空响应等不应被声明为 gzip。
type gzipResponseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	gzipBody    bool
	gz          *gzip.Writer
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	if g.wroteHeader {
		return
	}
	g.wroteHeader = true
	g.status = code
	ct := g.Header().Get("Content-Type")
	g.gzipBody = code >= 200 && code != http.StatusNoContent && code != http.StatusNotModified &&
		(strings.HasPrefix(ct, "application/json") || strings.HasPrefix(ct, "text/csv"))
	if !g.gzipBody {
		g.ResponseWriter.WriteHeader(code)
	}
}

func (g *gzipResponseWriter) Write(p []byte) (int, error) {
	if !g.wroteHeader {
		g.WriteHeader(http.StatusOK)
	}
	if !g.gzipBody {
		return g.ResponseWriter.Write(p)
	}
	if g.gz == nil {
		g.Header().Del("Content-Length")
		g.Header().Set("Content-Encoding", "gzip")
		g.ResponseWriter.WriteHeader(g.status)
		g.gz = gzip.NewWriter(g.ResponseWriter)
	}
	return g.gz.Write(p)
}

func (g *gzipResponseWriter) Close() error {
	if g.gz != nil {
		return g.gz.Close()
	}
	if g.wroteHeader && g.gzipBody {
		// 类型可压缩却没有正文（如 200 空 JSON）：不声明 gzip，直接收尾。
		g.ResponseWriter.WriteHeader(g.status)
	}
	return nil
}

func withCommon(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		// 本项目无外部资源依赖，可直接上严格 CSP；同时禁止被第三方 iframe 嵌入。
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; frame-ancestors 'none'")
		// 明文 HTTP 下浏览器会忽略该头；反向代理终止 TLS 时同样能生效。
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		} else {
			// 静态资源允许缓存但每次都要回源校验，避免升级后拿到旧 CSS/JS
			w.Header().Set("Cache-Control", "no-cache")
		}
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// ---------- 上报 ----------

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, 405, "method not allowed")
		return
	}
	// 限制请求体，防止内存滥用
	body := http.MaxBytesReader(w, r.Body, 64<<10)
	var rep model.Report
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rep); err != nil {
		fail(w, 400, "bad json")
		return
	}

	token := r.Header.Get("X-Node-Token")
	node, ok := s.st.NodeByToken(token, rep.NodeID)
	if !ok {
		fail(w, 401, "invalid token")
		return
	}

	// 服务端以自身时间校正，避免 Agent 时钟漂移污染时序
	rep.Metrics.TS = time.Now().Unix()
	s.st.AddMetrics(node.ID, rep.Host, sanitizeMetrics(rep.Metrics))

	writeJSON(w, 200, map[string]any{"ok": true})
}

// sanitizeMetrics 在入库前修正异常指标。
//
// Agent 是可能被攻破的一端，它自报的数字会进入内存聚合、历史快照和
// Prometheus 输出，因此不能直接信任：
//   - NaN/Inf 无法被 JSON 编码，一份坏数据会让 history.json 永远写不出去
//     （Save 持续失败），进而丢掉全部历史；
//   - 用量大于总量会让 UI 显示 >100% 或负剩余量；
//   - 负载为负数会污染图表纵轴。
func sanitizeMetrics(m model.Metrics) model.Metrics {
	m.CPUPct = clampRange(m.CPUPct, 0, 100)
	m.Load1 = clampRange(m.Load1, 0, 1e6)
	m.Load5 = clampRange(m.Load5, 0, 1e6)
	m.Load15 = clampRange(m.Load15, 0, 1e6)
	if m.MemUsed > m.MemTotal {
		m.MemUsed = m.MemTotal
	}
	if m.DiskUsed > m.DiskTotal {
		m.DiskUsed = m.DiskTotal
	}
	if m.SwapUsed > m.SwapTotal {
		m.SwapUsed = m.SwapTotal
	}
	if m.TCPConn < 0 {
		m.TCPConn = 0
	}
	if m.Procs < 0 {
		m.Procs = 0
	}
	return m
}

func clampRange(v, lo, hi float64) float64 {
	if math.IsNaN(v) || v < lo {
		return lo
	}
	if math.IsInf(v, 1) || v > hi {
		return hi
	}
	return v
}

// ---------- 视图 ----------

// buildNodeViews 构造仪表盘用的完整视图（含迷你曲线与告警计数）。
func (s *Server) buildNodeViews() []model.NodeView { return s.nodeViews(true) }

// buildNodeViewsLite 构造只含数值的视图，供 Prometheus 抓取等高频路径使用。
// 抓取每次都要跑，而 sparkline 需要按节点复制造型后的原始序列、
// 告警计数需要额外加锁统计 —— 这两项在抓取路径上完全用不到。
func (s *Server) buildNodeViewsLite() []model.NodeView { return s.nodeViews(false) }

func (s *Server) nodeViews(withExtras bool) []model.NodeView {
	nodes := s.st.Nodes()
	var alertByNode map[string]int
	if withExtras {
		alertByNode = s.st.ActiveAlertsByNode()
	}
	now := time.Now().Unix()
	out := make([]model.NodeView, 0, len(nodes))

	for _, n := range nodes {
		m, ts, _ := s.st.Latest(n.ID)
		state := store.NodeState(ts, now)

		v := model.NodeMeta(n)
		v.Host = s.st.Host(n.ID)
		v.Latest = m
		v.Online = state == model.NodeStateOnline
		v.State = state
		v.LastSeen = ts
		if withExtras {
			v.Alerts = alertByNode[n.ID]
			// 迷你曲线：最近 30 个点的 CPU
			series := s.st.Series(n.ID, now-1200)
			if len(series) > 30 {
				series = series[len(series)-30:]
			}
			spark := make([]model.Point, 0, len(series))
			for _, p := range series {
				spark = append(spark, model.Point{T: p.TS, V: p.CPUPct})
			}
			v.Spark = spark
		}
		out = append(out, v)
	}
	return out
}

func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.buildNodeViews())
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	views := s.buildNodeViews()
	ov := model.Overview{Nodes: len(views)}
	var cpuSum, memSum float64
	var cnt int

	for _, v := range views {
		switch v.State {
		case model.NodeStateOnline:
			ov.Online++
			cpuSum += v.Latest.CPUPct
			memSum += pctF(v.Latest.MemUsed, v.Latest.MemTotal)
			ov.TrafficIn += v.Latest.NetRx
			ov.TrafficOut += v.Latest.NetTx
			cnt++
		case model.NodeStateStale:
			ov.Stale++
		case model.NodeStateOffline:
			ov.Offline++
		case model.NodeStateNoData:
			ov.NoData++
		}
		// 成本只取决于价格，与是否登记到期日无关；
		// 否则未填到期日的资产会在总览里凭空消失。
		ov.MonthlyCost += monthly(v.Price, v.Currency, s.cnyRate)
		if v.ExpireAt != "" {
			if days, ok := daysUntil(v.ExpireAt); ok && days >= 0 && days <= 30 {
				ov.Expiring = append(ov.Expiring, v)
			}
		}
	}
	if cnt > 0 {
		ov.AvgCPU = cpuSum / float64(cnt)
		ov.AvgMem = memSum / float64(cnt)
	}
	ov.ActiveAlert = s.st.ActiveAlertCount()

	svcs := s.st.Services()
	ov.Services = len(svcs)
	for _, sv := range svcs {
		_, _, ok, has := s.st.ServiceLast(sv.ID)
		if has && !ok {
			ov.SvcDown++
		}
	}
	sort.Slice(ov.Expiring, func(i, j int) bool { return ov.Expiring[i].ExpireAt < ov.Expiring[j].ExpireAt })
	writeJSON(w, 200, ov)
}

func (s *Server) handleNodeDetail(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/nodes/")
	if rest == "" {
		s.handleNodes(w, r)
		return
	}
	parts := strings.Split(rest, "/")
	id := parts[0]

	n, ok := s.st.Node(id)
	if !ok {
		fail(w, 404, "node not found")
		return
	}

	// /api/nodes/{id}/series?range=1h|6h|24h|7d|30d|365d
	if len(parts) > 1 && parts[1] == "series" {
		rng := r.URL.Query().Get("range")
		if rng == "" {
			rng = "1h"
		}
		window, resolution, ok := historyRange(rng)
		if !ok {
			fail(w, 400, "unknown range: "+rng)
			return
		}
		since := time.Now().Unix() - window
		var pts any
		if resolution == store.HistoryResolutionRaw {
			pts = s.st.Series(id, since)
		} else {
			pts = s.st.HistorySeries(id, resolution, since)
		}
		writeJSON(w, 200, map[string]any{
			"node": n, "range": rng, "resolution": resolution, "points": pts,
		})
		return
	}

	now := time.Now().Unix()
	m, ts, _ := s.st.Latest(id)
	state := store.NodeState(ts, now)
	writeJSON(w, 200, map[string]any{
		"node":     n,
		"host":     s.st.Host(id),
		"latest":   m,
		"lastSeen": ts,
		"online":   state == model.NodeStateOnline,
		"state":    state,
	})
}

// historyRange 把查询范围映射为时间窗口与分辨率：
// 24h 以内使用原始 30s 点；更长范围逐级降采样，避免一次返回数十万点。
func historyRange(rng string) (window int64, resolution string, ok bool) {
	switch rng {
	case "1h":
		return int64(time.Hour / time.Second), store.HistoryResolutionRaw, true
	case "6h":
		return int64(6 * time.Hour / time.Second), store.HistoryResolutionRaw, true
	case "24h":
		return int64(24 * time.Hour / time.Second), store.HistoryResolutionRaw, true
	case "7d":
		return int64(7 * 24 * time.Hour / time.Second), store.HistoryResolution1m, true
	case "30d":
		return int64(30 * 24 * time.Hour / time.Second), store.HistoryResolution5m, true
	case "365d":
		return int64(365 * 24 * time.Hour / time.Second), store.HistoryResolution1h, true
	}
	return 0, "", false
}

func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	svcs := s.st.Services()
	type item struct {
		model.Service
		OK        bool       `json:"ok"`
		LatencyMs int        `json:"latencyMs"`
		CertDays  int        `json:"certDays"`
		Uptime    float64    `json:"uptime"` // 24h 可用率
		Spark     [][3]int64 `json:"spark"`
	}
	out := make([]item, 0, len(svcs))
	for _, sv := range svcs {
		_, lat, ok, cert, has := s.st.ServiceLastFull(sv.ID)
		hist := s.st.ServiceSeries(sv.ID)
		// Uptime 声明为 24h 可用率，而 ServiceSeries 返回整个环形缓冲
		// （2880 点，按探测间隔不同可能是 8h 或 48h），必须显式截断窗口。
		cutoff := time.Now().Unix() - 86400
		up, tot := 0, 0
		for _, h := range hist {
			if h[0] < cutoff {
				continue
			}
			tot++
			if h[2] == 1 {
				up++
			}
		}
		rate := 0.0
		if tot > 0 {
			rate = float64(up) / float64(tot) * 100
		}
		it := item{Service: *sv, LatencyMs: lat, Spark: hist, Uptime: rate, CertDays: -1}
		if has {
			it.OK = ok
			it.CertDays = cert
		}
		out = append(out, it)
	}
	// 异常服务排在最前，便于快速定位
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].OK != out[j].OK {
			return !out[i].OK
		}
		return out[i].Name < out[j].Name
	})
	writeJSON(w, 200, out)
}

func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	writeJSON(w, 200, map[string]any{
		"active":   s.st.ActiveAlertCount(),
		"items":    s.st.Alerts(200),
		"silences": s.st.Silences(now),
		"notify":   s.st.NotifyStats(),
		"now":      now,
	})
}

func (s *Server) handleRules(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.st.Rules())
}

func (s *Server) handleAssets(w http.ResponseWriter, r *http.Request) {
	nodes := s.st.Nodes()
	type row struct {
		ID       string  `json:"id"`
		Name     string  `json:"name"`
		Provider string  `json:"provider"`
		Region   string  `json:"region"`
		ExpireAt string  `json:"expireAt"`
		Days     int     `json:"days"`
		Price    float64 `json:"price"`
		Currency string  `json:"currency"`
		Monthly  float64 `json:"monthly"`
		OS       string  `json:"os"`
		hasExp   bool
	}
	rows := make([]row, 0, len(nodes))
	var monthSum float64
	for _, n := range nodes {
		d, hasExp := daysUntil(n.ExpireAt)
		mv := monthly(n.Price, n.Currency, s.cnyRate)
		monthSum += mv
		rows = append(rows, row{
			ID: n.ID, Name: n.Name, Provider: n.Provider, Region: n.Region,
			ExpireAt: n.ExpireAt, Days: d, Price: n.Price, Currency: n.Currency,
			Monthly: mv, OS: s.st.Host(n.ID).OS, hasExp: hasExp,
		})
	}
	// 有到期日的排在前面，按剩余天数升序（已过期/今天到期的 Days<=0 最靠前）；
	// 不能用 Days == 0 判断“无到期日”，否则今天到期的资产会被排到最后。
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].hasExp != rows[j].hasExp {
			return rows[i].hasExp
		}
		if !rows[i].hasExp {
			return false
		}
		return rows[i].Days < rows[j].Days
	})
	writeJSON(w, 200, map[string]any{"monthlyTotal": monthSum, "rows": rows})
}

// handleStatus 是公开状态页数据（不含任何敏感信息）。
//
// 脱敏原则（公开页可被任何人访问）：
//
//	· 不含 IP、服务商、价格、到期日、备注
//	· 不含主机名、内核、架构等指纹信息
//	· 仅保留：展示名、在线状态、三项使用率、运行时长、24h 可用率色块
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	views := s.buildNodeViews()
	svcs := s.st.Services()

	type pub struct {
		Name     string      `json:"name"`
		Group    string      `json:"group"` // 仅分组名，用于公开页归类展示
		Online   bool        `json:"online"`
		State    string      `json:"state"`
		CPU      float64     `json:"cpu"`
		Mem      float64     `json:"mem"`
		Disk     float64     `json:"disk"`
		Up       uint64      `json:"uptime"`
		Uptime24 float64     `json:"uptime24"` // 24 小时在线率（由采样点推算）
		Bars     []statusBar `json:"bars"`     // 24h 分段可用性（每段 30 分钟）
	}

	// 服务公开视图：仅名称 + 最近状态 + 24h 可用率
	type svcPub struct {
		Name    string  `json:"name"`
		OK      bool    `json:"ok"`
		Has     bool    `json:"has"` // 是否已有探测数据
		Uptime  float64 `json:"uptime"`
		Latency int     `json:"latencyMs"`
	}

	out := make([]pub, 0, len(views))
	for _, v := range views {
		p := pub{
			Name: v.Name, Group: v.Group, Online: v.Online, State: v.State,
			CPU:  round1(v.Latest.CPUPct),
			Mem:  round1(pctF(v.Latest.MemUsed, v.Latest.MemTotal)),
			Disk: round1(pctF(v.Latest.DiskUsed, v.Latest.DiskTotal)),
			Up:   v.Latest.Uptime,
		}

		// 24h 可用率与分段色块：按"采样点覆盖的在线时长"计算，
		// 不能简单用 成功点数/总点数 —— 那样只要节点还在上报就永远 100%。
		now := time.Now().Unix()
		p.Uptime24, p.Bars = uptime24(s.st.Series(v.ID, now-86400), now, v.Online)
		out = append(out, p)
	}

	now := time.Now().Unix()
	svcOut := make([]svcPub, 0, len(svcs))
	for _, sv := range svcs {
		_, lat, ok, has := s.st.ServiceLast(sv.ID)
		rate, _ := serviceUptime24(s.st.ServiceSeries(sv.ID), now)
		sp := svcPub{Name: sv.Name, Uptime: round1(rate), Has: has}
		if has {
			sp.OK = ok
			sp.Latency = lat
		}
		svcOut = append(svcOut, sp)
	}

	counts := map[string]int{
		model.NodeStateOnline:  0,
		model.NodeStateStale:   0,
		model.NodeStateOffline: 0,
		model.NodeStateNoData:  0,
	}
	for _, p := range out {
		if _, ok := counts[p.State]; ok {
			counts[p.State]++
		}
	}
	svcDown := 0
	for _, sp := range svcOut {
		if sp.Has && !sp.OK {
			svcDown++
		}
	}

	writeJSON(w, 200, map[string]any{
		"site":        statusTitle,
		"nodes":       out,
		"services":    svcOut,
		"online":      counts[model.NodeStateOnline],
		"stale":       counts[model.NodeStateStale],
		"offline":     counts[model.NodeStateOffline],
		"noData":      counts[model.NodeStateNoData],
		"total":       len(out),
		"svcDown":     svcDown,
		"generatedAt": time.Now().Unix(),
	})
}

// statusTitle 是公开页标题。
//
// 曾经这里读 GetSetting("status_title")，但全项目**没有任何写入该设置的入口**，
// 于是它永远返回默认值 —— 属于误导性死代码，已移除。
// 若要支持自定义标题，需同时补管理 API 的写入路径与前端表单项。
const statusTitle = "服务状态"

// handleStatusPage 渲染公开状态页 HTML（独立于管理界面，免登录）。
func (s *Server) handleStatusPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, statusPageHTML)
}

// ---------- 工具 ----------

func pctF(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) / float64(total) * 100
}

// statusBar 是 24h 可用性的 30 分钟色块。
type statusBar struct {
	TS int64 `json:"t"`
	OK bool  `json:"ok"`
}

// uptime24 按采样点覆盖的在线时长计算 24h 可用率。
//
// 每个采样点只在其后 store.OnlineWindow 秒内被视为在线，
// 超出窗口的空白时段计入分母 —— 否则"只要还在上报就 100%"，
// 无法反映真实中断。统计起点取 max(24h 前, 首个采样点)。
func uptime24(series []model.Metrics, now int64, onlineNow bool) (float64, []statusBar) {
	const segSec int64 = 1800 // 30 分钟 × 48 段
	const nseg = 48
	winStart := now - 86400
	if len(series) == 0 {
		if onlineNow {
			return 100, nil
		}
		return 0, nil
	}

	segOnline := make([]int64, nseg)
	segObserved := make([]int64, nseg)
	addSpan := func(dst []int64, from, to int64) {
		if from < winStart {
			from = winStart
		}
		if to > now {
			to = now
		}
		for from < to {
			idx := int((from - winStart) / segSec)
			if idx < 0 {
				idx = 0
			}
			if idx >= nseg {
				break
			}
			segEnd := winStart + int64(idx+1)*segSec
			next := segEnd
			if next > to {
				next = to
			}
			dst[idx] += next - from
			from = next
		}
	}

	obsStart := series[0].TS
	if obsStart < winStart {
		obsStart = winStart
	}
	for i, m := range series {
		if m.TS > now {
			break
		}
		from := m.TS
		if from < obsStart {
			from = obsStart
		}
		to := now
		if i+1 < len(series) {
			to = series[i+1].TS
		}
		if to > now {
			to = now
		}
		if to <= from {
			continue
		}
		addSpan(segObserved, from, to)
		onlineTo := from + store.OnlineWindow
		if onlineTo > to {
			onlineTo = to
		}
		addSpan(segOnline, from, onlineTo)
	}

	var totalObs, totalOnline int64
	bars := make([]statusBar, 0, nseg)
	for i := 0; i < nseg; i++ {
		totalObs += segObserved[i]
		totalOnline += segOnline[i]
		if segObserved[i] > 0 {
			// TS 取该段的结束时刻，与前端 (now-TS)/1800 的映射保持一致
			bars = append(bars, statusBar{
				TS: winStart + int64(i+1)*segSec,
				OK: segOnline[i]*2 >= segObserved[i],
			})
		}
	}
	if totalObs <= 0 {
		if onlineNow {
			return 100, bars
		}
		return 0, bars
	}
	up := float64(totalOnline) / float64(totalObs) * 100
	if up > 100 {
		up = 100
	}
	return round1(up), bars
}

// serviceUptime24 只统计最近 24 小时的探测结果。
func serviceUptime24(hist [][3]int64, now int64) (float64, bool) {
	since := now - 86400
	up, tot := 0, 0
	for _, h := range hist {
		if h[0] < since {
			continue
		}
		tot++
		if h[2] == 1 {
			up++
		}
	}
	if tot == 0 {
		return 0, false
	}
	return float64(up) / float64(tot) * 100, true
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }

func monthly(price float64, cur string, cnyRate float64) float64 {
	// 简化：按月成本 = 年付价格 / 12；CNY 按配置汇率折算为 USD 仅用于聚合展示
	if cur == "CNY" {
		if cnyRate <= 0 {
			cnyRate = 7.2
		}
		return price / 12 / cnyRate
	}
	return price / 12
}

func daysUntil(date string) (int, bool) {
	if date == "" {
		return 0, false
	}
	// 按本地日历日计算：直接 Parse（UTC）再减去当前时刻，
	// 会在非 UTC 时区把天数算偏，且 int() 截断会让“今天到期”和
	// “还有 23 小时”都变成 0。
	t, err := time.ParseInLocation("2006-01-02", date, time.Local)
	if err != nil {
		return 0, false
	}
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	target := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)
	return int(math.Round(target.Sub(today).Hours() / 24)), true
}

// itoa 供外部小工具使用（避免引入 strconv 到多处）。
func itoa(i int) string { return strconv.Itoa(i) }

// ---------- 管理 API ----------

// adminOK 校验 X-Admin-Token。未配置令牌时一律拒绝。
func (s *Server) adminOK(r *http.Request) bool {
	if s.adminToken == "" {
		return false
	}
	t := r.Header.Get("X-Admin-Token")
	return subtle.ConstantTimeCompare([]byte(t), []byte(s.adminToken)) == 1
}

// adminNodeView 是带 token 的节点视图，仅管理 API 返回。
type adminNodeView struct {
	model.Node
	Token string `json:"token"`
}

// validateNodeMeta 清理并校验节点台账字段，返回空字符串表示通过。
func validateNodeMeta(n *model.Node) string {
	n.Name = strings.TrimSpace(n.Name)
	n.Provider = strings.TrimSpace(n.Provider)
	n.Region = strings.TrimSpace(n.Region)
	n.Group = strings.TrimSpace(n.Group)
	n.Note = strings.TrimSpace(n.Note)
	n.ExpireAt = strings.TrimSpace(n.ExpireAt)
	n.Currency = strings.ToUpper(strings.TrimSpace(n.Currency))
	if n.Name == "" {
		return "name is required"
	}
	if n.Currency == "" {
		n.Currency = "USD"
	}
	if n.Currency != "USD" && n.Currency != "CNY" {
		return "currency must be USD or CNY"
	}
	if n.Price < 0 {
		return "price must be >= 0"
	}
	if n.ExpireAt != "" {
		if _, err := time.Parse("2006-01-02", n.ExpireAt); err != nil {
			return "expireAt must be YYYY-MM-DD"
		}
	}
	return ""
}

func (s *Server) handleAdminNodes(w http.ResponseWriter, r *http.Request) {

	switch r.Method {
	case http.MethodGet:
		nodes := s.st.Nodes()
		out := make([]adminNodeView, 0, len(nodes))
		for _, n := range nodes {
			out = append(out, adminNodeView{Node: *n, Token: n.Token})
		}
		writeJSON(w, 200, out)

	case http.MethodPost:
		body := http.MaxBytesReader(w, r.Body, 16<<10)
		var in model.Node
		dec := json.NewDecoder(body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			fail(w, 400, "bad json: "+err.Error())
			return
		}
		if msg := validateNodeMeta(&in); msg != "" {
			fail(w, 400, msg)
			return
		}
		if in.ID == "" {
			in.ID = store.NewID("n")
			// 全新节点：服务端生成上报令牌，不接受客户端指定，避免弱令牌
			in.Token = store.NewToken()
		} else {
			// 客户端指定了 ID：只允许创建新节点。已存在的 ID 必须走 PUT，
			// 否则一次 POST 会静默改写台账（含到期日、价格），
			// 而调用方还以为自己只是「新建」。
			if _, ok := s.st.Node(in.ID); ok {
				fail(w, 409, "节点 ID 已存在，更新请使用 PUT /api/admin/nodes/"+in.ID)
				return
			}
			in.Token = store.NewToken()
		}
		s.st.UpsertNode(&in)
		writeJSON(w, 201, adminNodeView{Node: in, Token: in.Token})

	default:
		fail(w, 405, "method not allowed")
	}
}

func (s *Server) handleAdminNodeByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/admin/nodes/")
	rotate := strings.HasSuffix(id, "/rotate-token")
	if rotate {
		id = strings.TrimSuffix(id, "/rotate-token")
	}
	if id == "" {
		fail(w, 400, "missing node id")
		return
	}
	if rotate {
		if r.Method != http.MethodPost {
			fail(w, 405, "method not allowed")
			return
		}
		s.handleRotateNodeToken(w, id)
		return
	}
	switch r.Method {
	case http.MethodGet:
		n, ok := s.st.Node(id)
		if !ok {
			fail(w, 404, "node not found")
			return
		}
		writeJSON(w, 200, adminNodeView{Node: *n, Token: n.Token})
	case http.MethodPut:
		old, ok := s.st.Node(id)
		if !ok {
			fail(w, 404, "node not found")
			return
		}
		body := http.MaxBytesReader(w, r.Body, 16<<10)
		var in model.Node
		dec := json.NewDecoder(body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			fail(w, 400, "bad json: "+err.Error())
			return
		}
		if msg := validateNodeMeta(&in); msg != "" {
			fail(w, 400, msg)
			return
		}
		updated := *old
		updated.Name = in.Name
		updated.Provider = in.Provider
		updated.Region = in.Region
		updated.Group = in.Group
		updated.Note = in.Note
		updated.ExpireAt = in.ExpireAt
		updated.Price = in.Price
		updated.Currency = in.Currency
		updated.Weight = in.Weight
		// ID 与 Token 来自原节点，避免一次台账编辑静默换掉 Agent 凭据。
		s.st.UpsertNode(&updated)
		writeJSON(w, 200, adminNodeView{Node: updated, Token: updated.Token})
	case http.MethodDelete:
		if _, ok := s.st.Node(id); !ok {
			fail(w, 404, "node not found")
			return
		}
		s.st.DeleteNode(id)
		writeJSON(w, 200, map[string]any{"deleted": id})
	default:
		fail(w, 405, "method not allowed")
	}
}

// handleRotateNodeToken 显式轮换节点上报令牌。
// 普通台账编辑不会换令牌；只有调用该端点才轮换，并立即落盘，
// 避免轮换后进程崩溃导致新令牌丢失、Agent 永久 401。
func (s *Server) handleRotateNodeToken(w http.ResponseWriter, id string) {
	old, ok := s.st.Node(id)
	if !ok {
		fail(w, 404, "node not found")
		return
	}
	updated := *old
	updated.Token = store.NewToken()
	s.st.UpsertNode(&updated)
	if err := s.st.Save(); err != nil {
		s.st.UpsertNode(old)
		fail(w, 500, "保存新令牌失败: "+err.Error())
		return
	}
	log.Printf("节点令牌已轮换 id=%s", id)
	writeJSON(w, 200, adminNodeView{Node: updated, Token: updated.Token})
}

// ---------- 服务探测管理 API ----------

func (s *Server) handleAdminServices(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, s.st.Services())

	case http.MethodPost:
		body := http.MaxBytesReader(w, r.Body, 16<<10)
		// Enabled 用指针接收，以区分"未提供"（默认启用）与"显式 false"
		var req struct {
			ID       string `json:"id"`
			NodeID   string `json:"nodeId"`
			Name     string `json:"name"`
			Type     string `json:"type"`
			Target   string `json:"target"`
			Interval int    `json:"interval"`
			Enabled  *bool  `json:"enabled"`
		}
		dec := json.NewDecoder(body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			fail(w, 400, "bad json: "+err.Error())
			return
		}
		in := model.Service{
			ID: req.ID, NodeID: req.NodeID, Name: req.Name,
			Type: req.Type, Target: req.Target, Interval: req.Interval,
			Enabled: true, // 默认启用
		}
		if req.Enabled != nil {
			in.Enabled = *req.Enabled
		}
		if strings.TrimSpace(in.Name) == "" {
			fail(w, 400, "name is required")
			return
		}
		typ := strings.ToLower(strings.TrimSpace(in.Type))
		if typ != "http" && typ != "https" && typ != "tcp" {
			fail(w, 400, "type 必须是 http / https / tcp")
			return
		}
		in.Type = typ
		in.Target = strings.TrimSpace(in.Target)
		if in.Target == "" {
			fail(w, 400, "target is required")
			return
		}
		if s.runner != nil {
			if err := s.runner.ValidateService(in); err != nil {
				fail(w, 400, err.Error())
				return
			}
		}
		if in.ID == "" {
			in.ID = store.NewID("s")
		}
		if in.Interval <= 0 {
			in.Interval = 60
		}
		if in.Interval < 10 {
			fail(w, 400, "interval 不能小于 10 秒")
			return
		}
		if in.Interval > 86400 {
			fail(w, 400, "interval 不能超过 86400 秒")
			return
		}
		s.st.UpsertService(&in)
		writeJSON(w, 201, in)

	default:
		fail(w, 405, "method not allowed")
	}
}

func (s *Server) handleAdminServiceByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/admin/services/")
	if id == "" {
		fail(w, 400, "missing service id")
		return
	}
	if r.Method != http.MethodDelete {
		fail(w, 405, "method not allowed")
		return
	}
	if !s.st.DeleteService(id) {
		fail(w, 404, "service not found")
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": id})
}

// ---------- 通知设置 API ----------

func (s *Server) handleAdminSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg, err := notify.Parse(s.st.GetSetting("notify"))
		warn := ""
		if err != nil {
			// 已保存的配置损坏时不能把设置页锁死：GET 一直 500，
			// 管理员既看不到表单也无法保存新配置，只能去改数据库文件。
			// 返回空白配置并附上提示，让「重新填写并保存」成为可走的恢复路径。
			cfg = &notify.Config{}
			warn = "已保存的通知配置损坏，已回退为空白配置：" + err.Error()
			log.Printf("通知配置解析失败，返回空白配置: %v", err)
		}
		out := map[string]any{
			"notify":    cfg.Redacted(),
			"channels":  cfg.EnabledChannels(),
			"defs":      notify.Defs(),
			"panelLink": s.panelLink,
		}
		if warn != "" {
			out["warning"] = warn
		}
		writeJSON(w, 200, out)

	case http.MethodPost:
		body := http.MaxBytesReader(w, r.Body, 32<<10)
		var in notify.Config
		dec := json.NewDecoder(body)
		if err := dec.Decode(&in); err != nil {
			fail(w, 400, "bad json: "+err.Error())
			return
		}
		in.Normalize()
		if err := in.Validate(); err != nil {
			fail(w, 400, err.Error())
			return
		}
		// 还原前端回传的脱敏占位符。旧配置损坏时按「首次配置」处理，
		// 否则上面那条 500/409 会把损坏状态永久固化，再也无法覆盖。
		old, err := notify.Parse(s.st.GetSetting("notify"))
		if err != nil {
			log.Printf("已保存的通知配置损坏，按首次配置覆盖: %v", err)
			old = nil
		}
		in.MergeSecrets(old)

		raw, err := json.Marshal(&in)
		if err != nil {
			fail(w, 500, "序列化失败")
			return
		}
		// 落盘与生效必须作为整体：并发保存交错会让两者指向不同配置。
		s.notifySaveMu.Lock()
		s.st.SetSetting("notify", string(raw))
		s.applyNotifyConfig(&in)
		s.notifySaveMu.Unlock()
		writeJSON(w, 200, map[string]any{"saved": true, "channels": in.EnabledChannels()})

	default:
		fail(w, 405, "method not allowed")
	}
}

// handleTestNotify 发一条测试通知，验证通道配置是否正确。
func (s *Server) handleTestNotify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, 405, "method not allowed")
		return
	}
	// 测试通知会真实外发，限制频率避免被拿来当扫描器或刷屏。
	// 注意这里只做检查、不记账：配额要在请求体与配置都解析通过后再扣，
	// 否则一次畸形请求就能让真正的测试在三秒内被 429 挡回。
	s.testMu.Lock()
	if wait := 3*time.Second - time.Since(s.lastTest); wait > 0 && !s.lastTest.IsZero() {
		s.testMu.Unlock()
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		fail(w, 429, "测试通知过于频繁，请稍后再试")
		return
	}
	s.testMu.Unlock()

	// 可选：只测某个通道（body 里带 channelId）
	var req struct {
		ChannelID string `json:"channelId"`
	}
	body := http.MaxBytesReader(w, r.Body, 4<<10)
	if err := json.NewDecoder(body).Decode(&req); err != nil && err != io.EOF {
		// 空 body 合法（测全部通道）；语法错误必须拒绝，
		// 否则畸形请求会被当成「无 channelId」并向所有通道群发。
		fail(w, 400, "请求体不是合法 JSON")
		return
	}

	cfg, err := notify.Parse(s.st.GetSetting("notify"))
	if err != nil {
		fail(w, 500, "配置解析失败: "+err.Error())
		return
	}
	// 测试消息无视开关与严重度阈值，强制投递
	testCfg := &notify.Config{
		Enabled:     true,
		MinSeverity: "info",
		Channels:    make([]notify.ChannelConfig, 0, len(cfg.Channels)),
	}
	for _, ch := range cfg.Channels {
		c := ch
		if req.ChannelID != "" && c.ID != req.ChannelID {
			continue
		}
		c.Enabled = true // 未启用的也测，便于配置阶段验证
		testCfg.Channels = append(testCfg.Channels, c)
	}
	if len(testCfg.Channels) == 0 {
		fail(w, 400, "没有可测试的通道，请先添加通道配置")
		return
	}
	// 所有校验都通过，才扣减这 3 秒一次的配额。
	s.testMu.Lock()
	s.lastTest = time.Now()
	s.testMu.Unlock()

	results := notify.Send(testCfg, notify.Message{
		Title:    "测试通知",
		Body:     "这是一条来自 Rollcall 的测试通知，收到即表示通道配置正确。",
		Severity: "info",
		NodeName: "Rollcall 控制端",
		RuleName: "连通性自检",
		TS:       time.Now().Unix(),
		Link:     s.panelLink,
	})
	writeJSON(w, 200, map[string]any{"results": results})
}

// ---------- 告警规则管理 API ----------

// ruleMetrics 是可作为规则依据的指标清单，前端据此渲染下拉框。
type ruleMetric struct {
	Value string  `json:"value"`
	Label string  `json:"label"`
	Unit  string  `json:"unit"`
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
	Step  float64 `json:"step"`
	Hint  string  `json:"hint"`
	// Op 为可用比较符；offline 类规则不需要阈值，由后端固定
	Op    string `json:"op"`
	NoVal bool   `json:"noVal"`
}

// handleRuleMeta 下发规则表单的元数据。虽为静态字典，仍与同前缀管理路由保持一致要求令牌。
func (s *Server) handleRuleMeta(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"metrics": []ruleMetric{
			{Value: "offline", Label: "节点失联", Unit: "秒", Op: "gt", NoVal: true,
				Hint: "超过指定时长未上报即触发；阈值固定为 1，无需填写"},
			{Value: "cpu", Label: "CPU 使用率", Unit: "%", Op: "gt", Min: 1, Max: 100, Step: 1,
				Hint: "常用阈值 85–95"},
			{Value: "mem", Label: "内存使用率", Unit: "%", Op: "gt", Min: 1, Max: 100, Step: 1,
				Hint: "常用阈值 85–90"},
			{Value: "disk", Label: "磁盘使用率", Unit: "%", Op: "gt", Min: 1, Max: 100, Step: 1,
				Hint: "常用阈值 80–90"},
			{Value: "load", Label: "1 分钟负载", Unit: "", Op: "gt", Min: 0.1, Max: 10000, Step: 0.1,
				Hint: "建议设为 CPU 核心数的 1–2 倍"},
		},
		"severities": []map[string]string{
			{"value": "critical", "label": "严重"},
			{"value": "warn", "label": "警告"},
			{"value": "info", "label": "信息"},
		},
		"durations": []map[string]any{
			{"value": 60, "label": "1 分钟"},
			{"value": 120, "label": "2 分钟"},
			{"value": 300, "label": "5 分钟"},
			{"value": 600, "label": "10 分钟"},
			{"value": 1800, "label": "30 分钟"},
		},
	})
}

// handleAdminRules 列出 / 创建告警规则。
func (s *Server) handleAdminRules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, s.st.Rules())

	case http.MethodPost:
		body := http.MaxBytesReader(w, r.Body, 16<<10)
		// Enabled 用指针接收，区分"未提供"（默认启用）与"显式 false"
		var req struct {
			ID        string   `json:"id"`
			Name      string   `json:"name"`
			Metric    string   `json:"metric"`
			Op        string   `json:"op"`
			Threshold *float64 `json:"threshold"`
			Duration  int      `json:"duration"`
			Severity  string   `json:"severity"`
			Enabled   *bool    `json:"enabled"`
			NodeIDs   []string `json:"nodeIds"`
			Groups    []string `json:"groups"`
		}
		dec := json.NewDecoder(body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			fail(w, 400, "bad json: "+err.Error())
			return
		}
		if strings.TrimSpace(req.Name) == "" {
			fail(w, 400, "name 不能为空")
			return
		}
		if !validMetric(req.Metric) {
			fail(w, 400, "metric 无效，可选 offline/cpu/mem/disk/load")
			return
		}
		severity := strings.TrimSpace(req.Severity)
		if severity == "" {
			severity = "warn"
		}
		if !validSeverity(severity) {
			fail(w, 400, "severity 无效，可选 info/warn/critical")
			return
		}

		// offline 规则不需要阈值；其余指标必须提供且在合理区间
		threshold := 0.0
		if req.Metric != "offline" {
			if req.Threshold == nil {
				fail(w, 400, "该指标必须提供 threshold")
				return
			}
			threshold = *req.Threshold
			lo, hi := thresholdRange(req.Metric)
			if threshold < lo || threshold > hi {
				fail(w, 400, fmt.Sprintf("threshold 必须在 %v – %v 之间", lo, hi))
				return
			}
		}

		duration := req.Duration
		if duration < 0 {
			fail(w, 400, "duration 不能为负")
			return
		}
		if duration > 86400 {
			fail(w, 400, "duration 不能超过 86400 秒")
			return
		}

		rule := &model.AlertRule{
			ID:        req.ID,
			Name:      strings.TrimSpace(req.Name),
			Metric:    req.Metric,
			Op:        normalizeOp(req.Op, req.Metric),
			Threshold: threshold,
			Duration:  duration,
			Severity:  severity,
			Enabled:   true,
			NodeIDs:   normalizeScope(req.NodeIDs),
			Groups:    normalizeScope(req.Groups),
		}
		if req.Enabled != nil {
			rule.Enabled = *req.Enabled
		}
		if rule.ID == "" {
			rule.ID = store.NewID("r")
		}
		s.st.UpsertRule(rule)
		writeJSON(w, 201, rule)

	default:
		fail(w, 405, "method not allowed")
	}
}

// handleAdminRuleByID 修改 / 删除单条规则。
func (s *Server) handleAdminRuleByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/admin/rules/")
	if id == "" {
		fail(w, 400, "missing rule id")
		return
	}

	existing, ok := s.ruleByID(id)
	if !ok {
		fail(w, 404, "rule not found")
		return
	}

	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, existing)

	case http.MethodPut, http.MethodPost:
		body := http.MaxBytesReader(w, r.Body, 16<<10)
		var req struct {
			Name      string    `json:"name"`
			Metric    string    `json:"metric"`
			Op        string    `json:"op"`
			Threshold *float64  `json:"threshold"`
			Duration  *int      `json:"duration"`
			Severity  string    `json:"severity"`
			Enabled   *bool     `json:"enabled"`
			NodeIDs   *[]string `json:"nodeIds"`
			Groups    *[]string `json:"groups"`
		}
		dec := json.NewDecoder(body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			fail(w, 400, "bad json: "+err.Error())
			return
		}

		// 逐字段覆盖，未提供的保持原值
		updated := *existing
		if strings.TrimSpace(req.Name) != "" {
			updated.Name = strings.TrimSpace(req.Name)
		}
		if req.Metric != "" {
			if !validMetric(req.Metric) {
				fail(w, 400, "metric 无效")
				return
			}
			updated.Metric = req.Metric
		}
		if req.Severity != "" {
			if !validSeverity(req.Severity) {
				fail(w, 400, "severity 无效")
				return
			}
			updated.Severity = req.Severity
		}
		if req.Op != "" {
			if req.Op != "gt" && req.Op != "lt" {
				fail(w, 400, "op 必须是 gt 或 lt")
				return
			}
			updated.Op = req.Op
		}
		if req.Threshold != nil {
			// offline 规则的阈值是「失联多少秒」，与 cpu/mem 的量纲无关。
			// 原来这里借道 thresholdRange 拿到 (0,0)，导致前端整体回传规则时
			// 被 400 拒绝；改为按秒数区间单独校验，并归一化为 0。
			if updated.Metric == "offline" {
				if *req.Threshold < 0 || *req.Threshold > 86400*7 {
					fail(w, 400, "offline 规则的 threshold 是失联秒数，必须在 0 – 604800 之间")
					return
				}
				updated.Threshold = 0
			} else {
				lo, hi := thresholdRange(updated.Metric)
				if *req.Threshold < lo || *req.Threshold > hi {
					fail(w, 400, fmt.Sprintf("threshold 必须在 %v – %v 之间", lo, hi))
					return
				}
				updated.Threshold = *req.Threshold
			}
		}
		if req.Duration != nil {
			if *req.Duration < 0 || *req.Duration > 86400 {
				fail(w, 400, "duration 必须在 0 – 86400 秒之间")
				return
			}
			updated.Duration = *req.Duration
		}
		if req.Enabled != nil {
			updated.Enabled = *req.Enabled
		}
		if req.NodeIDs != nil {
			updated.NodeIDs = normalizeScope(*req.NodeIDs)
		}
		if req.Groups != nil {
			updated.Groups = normalizeScope(*req.Groups)
		}
		if updated.Metric == "offline" {
			updated.Threshold = 0
			updated.Op = "gt"
		} else {
			// 只改 metric 时也要用最终合并后的规则重新校验阈值。
			// 否则 offline（阈值 0）切到 cpu/mem/disk 后会保留 0，
			// 变成“所有采样都越界”的持续误报。
			lo, hi := thresholdRange(updated.Metric)
			if updated.Threshold < lo || updated.Threshold > hi {
				fail(w, 400, fmt.Sprintf("切换 metric 后 threshold %v 超出范围 %v – %v，请同时提供新的 threshold", updated.Threshold, lo, hi))
				return
			}
		}

		s.st.UpsertRule(&updated)
		writeJSON(w, 200, updated)

	case http.MethodDelete:
		active := s.st.ActiveAlertsByRule(id)
		if !s.st.DeleteRule(id) {
			fail(w, 404, "rule not found")
			return
		}
		if s.engine != nil {
			// 传 ruleID：快照与 DeleteRule 之间新开的告警也属于已删除规则，
			// 只按快照清会留下永不自愈的幽灵条目。
			s.engine.RecoverRuleClosed(id, active, "规则已删除")
		}
		writeJSON(w, 200, map[string]any{"deleted": id})

	default:
		fail(w, 405, "method not allowed")
	}
}

func (s *Server) ruleByID(id string) (*model.AlertRule, bool) {
	for _, r := range s.st.Rules() {
		if r.ID == id {
			return r, true
		}
	}
	return nil, false
}

// normalizeScope 清理规则定向范围：去空白、去重、丢弃空值。
func normalizeScope(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
		if len(out) >= 200 {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func validMetric(m string) bool {
	switch m {
	case "offline", "cpu", "mem", "disk", "load":
		return true
	}
	return false
}

func validSeverity(sv string) bool {
	switch sv {
	case "info", "warn", "critical":
		return true
	}
	return false
}

// thresholdRange 返回各指标的合法阈值区间。
func thresholdRange(metric string) (float64, float64) {
	switch metric {
	case "cpu", "mem", "disk":
		return 1, 100
	case "load":
		return 0.1, 10000
	case "offline":
		return 0, 0
	}
	return 0, 0
}

// normalizeOp 归一化比较符：offline 固定 gt；其余默认 gt。
func normalizeOp(op, metric string) string {
	if metric == "offline" {
		return "gt"
	}
	if op == "lt" {
		return "lt"
	}
	return "gt"
}

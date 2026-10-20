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

	"github.com/motao123/rollcall/internal/alert"
	"github.com/motao123/rollcall/internal/auth"
	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/notify"
	"github.com/motao123/rollcall/internal/outbox"
	"github.com/motao123/rollcall/internal/probe"
	"github.com/motao123/rollcall/internal/selfupdate"
	"github.com/motao123/rollcall/internal/store"
	"github.com/motao123/rollcall/internal/version"
	// 包内已有一个同名函数 sanitize（清理 Content-Disposition 文件名），
	// 故此处带别名引入。
	sanitizetext "github.com/motao123/rollcall/internal/sanitize"
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
	user       *auth.User
	userPath   string
	sessions   *auth.Sessions
	panelLink  string
	cnyRate    float64
	limiter    *authFailureLimiter
	outbox     *outbox.Manager
	notifyErr  error

	// userMu 保护 user 指针：账号修改时整体替换副本，读方拿到指针后
	// 可安全使用（User 一经发布不再原地修改）。
	userMu sync.RWMutex
	// accountMu 串行化「校验当前密码 + 落盘 + 替换指针」，
	// 避免两个并发修改各自基于旧账号互相覆盖。
	accountMu sync.Mutex

	// trustedProxies 是允许提交 X-Forwarded-For 的反向代理网段。
	// 为空时只信任回环（见 defaultTrustedProxies）。
	trustedProxies []*net.IPNet

	// agentDir 是 Agent 二进制分发目录（-agent-dir）。
	// 为空表示不提供「一条命令接入」，下载接口会明确报错。
	agentDir string

	// dataDir 与 saveInterval 用于 /readyz 判断磁盘可写与快照是否新鲜。
	dataDir      string
	saveInterval time.Duration
	ready        readyProbe

	testMu   sync.Mutex
	lastTest time.Time

	// revokedMu 保护 revoked：会话令牌指纹 -> 过期时间。
	// HMAC 会话是无状态的，只清 Cookie 无法阻止被窃取的令牌继续使用，
	// 登出时把令牌指纹记入持久化吊销列表。
	revokedMu sync.Mutex
	revoked   map[string]int64

	// events 是 SSE 推送的变更广播器（见 stream.go）。
	events *eventHub

	// startedAt 用于「实例健康」卡片显示运行时长。
	startedAt time.Time
	// reports 统计最近 60 秒的上报次数（滑窗），用于看采集是否正常。
	reports reportCounter

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
	User       *auth.User      // 面板管理员账号；nil 时仅管理令牌可用
	UserPath   string          // admin-user.json 路径；为空则不允许在线修改账号
	Sessions   *auth.Sessions  // 面板会话签名器
	PanelLink  string          // 面板对外地址，拼进通知正文
	CNYRate    float64         // CNY→USD 折算汇率，<=0 时使用 7.2
	Outbox     *outbox.Manager // 通知投递队列；nil 时不外发通知
	AgentDir   string          // Agent 二进制分发目录；空则不提供下载
	// DataDir 与 SaveInterval 供 /readyz 做深度检查；SaveInterval<=0 时跳过时效判断。
	DataDir      string
	SaveInterval time.Duration
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
		user: cfg.User, userPath: cfg.UserPath, sessions: cfg.Sessions,
		panelLink: cfg.PanelLink, cnyRate: rate,
		limiter: newAuthFailureLimiter(), outbox: cfg.Outbox,
		agentDir:       cfg.AgentDir,
		trustedProxies: trusted,
		dataDir:        cfg.DataDir,
		saveInterval:   cfg.SaveInterval,
		events:         newEventHub(),
		startedAt:      time.Now()}
	srv.loadRevokedSessions()
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
		s.engine.SetNotifier(func(m notify.Message) { s.outbox.Enqueue(m) })
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
	authLogin                         // 登录/会话端点本身；失败限速独立分桶
	authNodeToken                     // handler 内校验 X-Node-Token（Agent 上报）
	authReadonly                      // 管理令牌、Basic 或面板会话（只读仪表盘）
	authAdmin                         // 管理令牌，或面板会话 + 同源头（写操作、导出）
	authMetrics                       // 管理令牌或面板凭据（Prometheus 抓取）
)

func (l authLevel) valid() bool { return l > authUnclassified && l <= authMetrics }

func (l authLevel) String() string {
	switch l {
	case authPublic:
		return "public"
	case authLogin:
		return "login"
	case authNodeToken:
		return "node-token"
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
func (l authLevel) protected() bool {
	switch l {
	case authReadonly, authAdmin, authMetrics:
		return true
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
		// 深度健康检查：存储可用且快照在正常落盘才算就绪。
		// 升级脚本以它决定是否回滚。
		{"/readyz", authPublic, http.HandlerFunc(s.handleReadyz)},
		{"/status", authPublic, http.HandlerFunc(s.handleStatusPage)},
		{"/status.js", authPublic, static},
		{"/api/status", authPublic, http.HandlerFunc(rejectNonGET(s.handleStatus))},
		// 节点详情与事件时间线同样是公开页的一部分：
		// 字段在 handler 内部按 StatusPage 配置的可见性裁剪，
		// 因此可以直接标记为公开，不必再套一层鉴权。
		{"/api/status/node/", authPublic, http.HandlerFunc(rejectNonGET(s.handleStatusNode))},
		{"/api/status/incidents", authPublic, http.HandlerFunc(rejectNonGET(s.handleStatusIncidents))},
		// 一条命令接入：脚本与二进制都公开（脚本不含密钥，token 由调用方传参）
		{"/agent/install.sh", authPublic, http.HandlerFunc(s.handleAgentInstall)},
		{"/agent/download", authPublic, http.HandlerFunc(s.handleAgentDownload)},
		{"/agent/download.sha256", authPublic, http.HandlerFunc(s.handleAgentChecksum)},
		{"/api/theme", authPublic, http.HandlerFunc(s.handleTheme)},

		// 面板登录：登录失败按独立 scope 限速，不会拖累公开页。
		{"/api/auth/login", authLogin, http.HandlerFunc(s.handleAuthLogin)},
		{"/api/auth/session", authLogin, http.HandlerFunc(s.handleAuthSession)},
		{"/api/auth/logout", authLogin, http.HandlerFunc(s.handleAuthLogout)},

		// Agent 上报：handler 内校验 X-Node-Token
		{"/api/report", authNodeToken, http.HandlerFunc(s.handleReport)},

		// 仪表盘只读数据包含主机名、内核、服务商、价格等资产信息，
		// 不能像公开状态页那样匿名下发。
		{"/api/overview", authReadonly, http.HandlerFunc(s.handleOverview)},
		{"/api/live", authReadonly, http.HandlerFunc(s.handleLive)},
		{"/api/stream", authAdmin, http.HandlerFunc(s.handleStream)},
		{"/api/nodes", authReadonly, http.HandlerFunc(s.handleNodes)},
		{"/api/nodes/", authReadonly, http.HandlerFunc(s.handleNodeDetail)},
		{"/api/services", authReadonly, http.HandlerFunc(s.handleServices)},
		{"/api/alerts", authReadonly, http.HandlerFunc(s.handleAlerts)},
		{"/api/rules", authReadonly, http.HandlerFunc(s.handleRules)},
		{"/api/assets", authReadonly, http.HandlerFunc(s.handleAssets)},
		{"/api/renewals", authReadonly, http.HandlerFunc(s.handleRenewals)},
		{"/api/compare", authReadonly, http.HandlerFunc(s.handleCompare)},

		// 管理写操作与运营数据导出：管理令牌或面板会话
		{"/api/admin/nodes", authAdmin, http.HandlerFunc(s.handleAdminNodes)},
		{"/api/admin/nodes/batch", authAdmin, http.HandlerFunc(s.handleAdminNodesBatch)},
		{"/api/admin/nodes/", authAdmin, http.HandlerFunc(s.handleAdminNodeByID)},
		{"/api/admin/services", authAdmin, http.HandlerFunc(s.handleAdminServices)},
		{"/api/admin/services/", authAdmin, http.HandlerFunc(s.handleAdminServiceByID)},
		{"/api/admin/settings", authAdmin, http.HandlerFunc(s.handleAdminSettings)},
		{"/api/admin/health", authAdmin, http.HandlerFunc(s.handleAdminHealth)},
		{"/api/admin/renewals", authAdmin, http.HandlerFunc(s.handleAdminRenewals)},
		{"/api/admin/renewals/", authAdmin, http.HandlerFunc(s.handleAdminRenewalByID)},
		// 管理员账号资料与密码：除会话/令牌外还要求当前密码，
		// 防止被窃取的会话直接接管账号。
		{"/api/admin/account", authAdmin, http.HandlerFunc(s.handleAdminAccount)},
		{"/api/admin/account/password", authAdmin, http.HandlerFunc(s.handleAdminAccountPassword)},
		{"/api/admin/account/totp", authAdmin, http.HandlerFunc(s.handleAdminAccountTOTP)},
		{"/api/admin/tokens", authAdmin, http.HandlerFunc(s.handleAdminTokens)},
		{"/api/admin/tokens/", authAdmin, http.HandlerFunc(s.handleAdminTokenByID)},
		{"/api/admin/backup", authAdmin, http.HandlerFunc(s.handleBackup)},
		{"/api/admin/restore", authAdmin, http.HandlerFunc(s.handleRestore)},
		{"/api/admin/theme", authAdmin, http.HandlerFunc(s.handleAdminTheme)},
		{"/api/admin/test-notify", authAdmin, http.HandlerFunc(s.handleTestNotify)},
		{"/api/admin/rules", authAdmin, http.HandlerFunc(s.handleAdminRules)},
		{"/api/admin/rules/", authAdmin, http.HandlerFunc(s.handleAdminRuleByID)},
		{"/api/admin/rule-meta", authAdmin, http.HandlerFunc(s.handleRuleMeta)},
		{"/api/admin/silences", authAdmin, http.HandlerFunc(s.handleAdminSilences)},
		{"/api/admin/silences/", authAdmin, http.HandlerFunc(s.handleAdminSilenceByScope)},
		// 公开状态页的展示配置、维护窗口与人工事件：
		// 这些直接影响匿名访客看到的内容，必须是管理写操作。
		{"/api/admin/status-page", authAdmin, http.HandlerFunc(s.handleAdminStatusPage)},
		{"/api/admin/maintenance", authAdmin, http.HandlerFunc(s.handleAdminMaintenance)},
		{"/api/admin/maintenance/", authAdmin, http.HandlerFunc(s.handleAdminMaintenanceByID)},
		{"/api/admin/incidents", authAdmin, http.HandlerFunc(s.handleAdminIncidents)},
		{"/api/admin/incidents/", authAdmin, http.HandlerFunc(s.handleAdminIncidentByID)},
		{"/api/admin/notify/jobs", authAdmin, http.HandlerFunc(s.handleAdminNotifyJobs)},
		{"/api/admin/notify/jobs/", authAdmin, http.HandlerFunc(s.handleAdminNotifyJobByID)},
		{"/api/export/", authAdmin, http.HandlerFunc(s.handleExport)},
		{"/metrics", authMetrics, http.HandlerFunc(s.handleMetrics)},

		// 面板静态资源必须公开：登录页本身也要能加载。
		// 数据接口由会话/令牌保护，静态资源不含敏感信息。
		{"/", authPublic, static},
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
	static := withStaticETag(sub, http.FileServer(http.FS(sub)))

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
	case authPublic, authLogin, authNodeToken:
		return h
	case authReadonly:
		return s.requireReadonly(h)
	case authAdmin:
		return s.requireAdmin(h)
	case authMetrics:
		return s.requireMetrics(h)
	default:
		// validateRoutes 已拦截；这里保留兜底，绝不静默放行
		panic("rollcall: unclassified route")
	}
}

// requireReadonly 保护仪表盘只读接口。
//
// 管理令牌用于自动化；面板会话用于浏览器；-auth 的 Basic 凭据保留兼容。
func (s *Server) requireReadonly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.adminTokenOK(r) || s.apiTokenOK(r, false) || s.panelBasicOK(r) || s.sessionOK(r) {
			h.ServeHTTP(w, r)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// requireAdmin 保护管理写操作与导出。
//
// 管理令牌请求带自定义头，天然不受 CSRF 影响；会话请求必须额外带
// X-Requested-With，配合 SameSite=Strict Cookie 阻断跨站表单提交。
func (s *Server) requireAdmin(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.adminOK(r) || s.apiTokenOK(r, true) {
			h.ServeHTTP(w, r)
			return
		}
		if s.sessionOK(r) {
			if sessionMutationAllowed(r) {
				h.ServeHTTP(w, r)
				return
			}
			fail(w, http.StatusForbidden, "missing X-Requested-With")
			return
		}
		fail(w, http.StatusUnauthorized, "invalid admin token")
	})
}

// sessionMutationAllowed 要求会话写操作携带自定义头。
// 跨站表单无法设置该头；跨站 fetch 会先触发 CORS 预检，而服务端不返回 CORS 头。
func sessionMutationAllowed(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Requested-With")), "XMLHttpRequest")
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
		if !gzipPath(r.URL.Path) {
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

// Unwrap 让 http.ResponseController 拿到下层 ResponseWriter，
// 这样 SetWriteDeadline 之类的连接级操作能穿透中间件。
func (g *gzipResponseWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

// Flush 透传：即使某条路径被 gzip 包装，也应能把已写内容立即推出，
// 而不是等缓冲区满。SSE 路径已在 gzipPath 里排除，这里是防御性的。
func (g *gzipResponseWriter) Flush() {
	if g.gz != nil {
		_ = g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	if g.wroteHeader {
		return
	}
	g.wroteHeader = true
	g.status = code
	ct := g.Header().Get("Content-Type")
	g.gzipBody = code >= 200 && code != http.StatusNoContent && code != http.StatusNotModified &&
		compressibleContentType(ct)
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

	// 只统计通过鉴权的上报：健康卡片用它与预期值对比判断采集是否掉队，
	// 未认证请求（扫描器、错误配置的 Agent）混进来会让速率虚高。
	s.reports.hit(time.Now().Unix())

	// Agent 版本走请求头，避免给上报 JSON 加字段：旧服务端用
	// DisallowUnknownFields 解码，先升级 Agent 会让整份上报被拒。
	if v := sanitizetext.Text(strings.TrimSpace(r.Header.Get("X-Agent-Version")), 64); v != "" {
		s.st.SetAgentVersion(node.ID, v)
	}

	// 服务端以自身时间校正，避免 Agent 时钟漂移污染时序
	rep.Metrics.TS = time.Now().Unix()
	s.st.AddMetrics(node.ID, sanitizeHost(rep.Host), sanitizeMetrics(rep.Metrics))

	// 唤醒 SSE 订阅者：面板不必再等下一次轮询，上报落地即可刷新。
	s.publishChange()

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
	// 累计流量计数器参与历史差分，异常大的值（或 MaxUint64）会让
	// 单桶增量溢出/爆表；1 EiB 远高于任何真实网卡的生命周期总量。
	const maxNetCounter = uint64(1) << 60
	if m.NetRx > maxNetCounter {
		m.NetRx = maxNetCounter
	}
	if m.NetTx > maxNetCounter {
		m.NetTx = maxNetCounter
	}
	// 运行时长同样是 Agent 自报值，异常值会进入 Prometheus 与导出；
	// 100 年足够覆盖任何真实主机，又能挡住 uint64 上限。
	const maxUptime = uint64(100 * 365 * 24 * 60 * 60)
	if m.Uptime > maxUptime {
		m.Uptime = maxUptime
	}
	if len(m.Mounts) > 16 {
		m.Mounts = m.Mounts[:16]
	}
	for i := range m.Mounts {
		m.Mounts[i].Path = sanitizetext.Text(m.Mounts[i].Path, 200)
		if m.Mounts[i].Used > m.Mounts[i].Total {
			m.Mounts[i].Used = m.Mounts[i].Total
		}
	}
	if len(m.NICs) > 8 {
		m.NICs = m.NICs[:8]
	}
	for i := range m.NICs {
		m.NICs[i].Name = sanitizetext.Text(m.NICs[i].Name, 32)
		if m.NICs[i].Rx > maxNetCounter {
			m.NICs[i].Rx = maxNetCounter
		}
		if m.NICs[i].Tx > maxNetCounter {
			m.NICs[i].Tx = maxNetCounter
		}
	}
	if m.DiskReadBytes > maxNetCounter {
		m.DiskReadBytes = maxNetCounter
	}
	if m.DiskWriteBytes > maxNetCounter {
		m.DiskWriteBytes = maxNetCounter
	}
	return m
}

// sanitizeHost 清洗 Agent 自报的文本字段。
//
// 这些字符串会原样进入内存快照、状态页、CSV 导出、Prometheus 输出与服务端
// 日志。控制字符能伪造日志换行（把一条记录伪装成两条），双向控制符能让文本
// 在编辑器与终端里显示成与真实字节序不同的样子 —— 而告警处置恰恰是靠人读
// 这些文本做判断的，所以必须在入口就清洗掉。
//
// 每个字段单独限长：CPU 型号这类字段在某些机器上会很长，统一截断即可，
// 不需要为此拒绝整份上报。
func sanitizeHost(h model.HostInfo) model.HostInfo {
	h.Hostname = sanitizetext.Text(h.Hostname, 128)
	h.OS = sanitizetext.Text(h.OS, 64)
	h.Kernel = sanitizetext.Text(h.Kernel, 128)
	h.Arch = sanitizetext.Text(h.Arch, 32)
	h.CPUModel = sanitizetext.Text(h.CPUModel, 128)
	h.Virt = sanitizetext.Text(h.Virt, 32)
	return h
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
		v.Traffic = s.st.TrafficUsage(n.ID)
		v.Online = state == model.NodeStateOnline
		v.State = state
		v.LastSeen = ts
		v.AgentVersion = n.AgentVersion
		v.AgentOutdated = agentVersionOutdated(n.AgentVersion)
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
		"node":          n,
		"host":          s.st.Host(id),
		"latest":        m,
		"traffic":       s.st.TrafficUsage(id),
		"lastSeen":      ts,
		"online":        state == model.NodeStateOnline,
		"state":         state,
		"agentOutdated": agentVersionOutdated(n.AgentVersion),
		"serverVersion": version.Version,
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
		LossPct   float64    `json:"lossPct"`
		JitterMs  float64    `json:"jitterMs"`
		Uptime    float64    `json:"uptime"` // 24h 可用率
		Spark     [][3]int64 `json:"spark"`
	}
	out := make([]item, 0, len(svcs))
	now := time.Now().Unix()
	for _, sv := range svcs {
		_, lat, ok, cert, has := s.st.ServiceLastFull(sv.ID)
		st := s.st.ServiceStats(sv.ID, now-3600)
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
		it := item{Service: *sv, LatencyMs: lat, Spark: hist, Uptime: rate, CertDays: -1,
			LossPct: st.LossPct, JitterMs: st.JitterMs}
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
	cfg := s.st.StatusPage()
	// 公开页自己按 24h 序列计算迷你曲线与可用率，不需要 NodeView 的
	// 告警计数和 30 点 sparkline —— 后者会为每个节点额外取一次序列。
	views := s.buildNodeViewsLite()
	svcs := s.st.Services()
	vis := cfg.Visibility

	// 服务公开视图：仅名称 + 最近状态 + 24h 可用率
	type svcPub struct {
		Name         string        `json:"name"`
		OK           bool          `json:"ok"`
		Has          bool          `json:"has"` // 是否已有探测数据
		Uptime       float64       `json:"uptime"`
		FailRate     float64       `json:"failRate,omitempty"`
		LossPct      float64       `json:"lossPct"`
		JitterMs     float64       `json:"jitterMs"`
		Latency      int           `json:"latencyMs"`
		LatencySpark []model.Point `json:"latencySpark,omitempty"` // 24h 成功探测延迟
	}

	now := time.Now().Unix()
	maintGlobal, maintNodes := s.st.MaintenanceAt(now)
	maintAll := maintGlobal != nil

	out := make([]statusNodePub, 0, len(views))
	for _, v := range views {
		// 24h 可用率与分段色块：按"采样点覆盖的在线时长"计算，
		// 不能简单用 成功点数/总点数 —— 那样只要节点还在上报就永远 100%。
		series := s.st.Series(v.ID, now-86400)
		up, bars := uptime24(series, now, v.Online)

		p := statusNodePub{
			HID: s.st.PublicHandle(v.ID), Name: v.Name, Group: store.VisibleGroup(v.Group),
			Online: v.Online, State: v.State,
			CPU:      round1(v.Latest.CPUPct),
			Mem:      round1(pctF(v.Latest.MemUsed, v.Latest.MemTotal)),
			Disk:     round1(pctF(v.Latest.DiskUsed, v.Latest.DiskTotal)),
			Up:       v.Latest.Uptime,
			Uptime24: round1(up),
			Bars:     bars,
			LastSeen: v.LastSeen,
		}
		if vis.Metrics {
			p.MemUsed, p.MemTotal = v.Latest.MemUsed, v.Latest.MemTotal
			p.DiskUsed, p.DiskTotal = v.Latest.DiskUsed, v.Latest.DiskTotal
		} else {
			// 未开启指标公开时列表也不应带出数值：
			// 前端拿不到就什么都不画，比下发 0 让人误以为"负载为零"更诚实。
			p.CPU, p.Mem, p.Disk = 0, 0, 0
		}
		if !vis.Uptime {
			p.Up = 0
		}
		if vis.Load {
			p.Load1 = floatPtr(round2(v.Latest.Load1))
			p.Load5 = floatPtr(round2(v.Latest.Load5))
			p.Load15 = floatPtr(round2(v.Latest.Load15))
		}
		if vis.Traffic {
			p.NetRx, p.NetTx = netRates(series)
			p.TotRx, p.TotTx = v.Latest.NetRx, v.Latest.NetTx
		}
		if vis.Swap {
			p.SwapUsed, p.SwapTotal = v.Latest.SwapUsed, v.Latest.SwapTotal
		}
		if vis.Procs {
			p.Procs = v.Latest.Procs
		}
		if vis.TCPConn {
			p.TCPConn = v.Latest.TCPConn
		}
		if vis.Cores {
			p.Cores = v.Host.Cores
		}
		if vis.OS {
			p.OS = v.Host.OS
		}
		if vis.Arch {
			p.Arch = v.Host.Arch
		}
		if vis.Virt {
			p.Virt = v.Host.Virt
		}
		if vis.Region {
			p.Region = v.Region
		}
		if vis.Provider {
			p.Provider = v.Provider
		}
		if vis.Country {
			p.Country = v.Country
		}
		// 迷你曲线：24h 序列降采样到 ~48 点，够画一条趋势线又不拖慢首屏
		if vis.Metrics && len(series) > 1 {
			step := len(series) / 48
			if step < 1 {
				step = 1
			}
			spark := make([]model.Point, 0, 48)
			for i := 0; i < len(series); i += step {
				spark = append(spark, model.Point{T: series[i].TS, V: round1(series[i].CPUPct)})
			}
			p.Spark = spark
		}
		if maintAll {
			p.MaintenanceTitle = maintGlobal.Title
		} else if m, ok := maintNodes[v.ID]; ok && m != nil {
			p.MaintenanceTitle = m.Title
		}
		out = append(out, p)
	}

	svcOut := make([]svcPub, 0, len(svcs))
	for _, sv := range svcs {
		hist := s.st.ServiceSeries(sv.ID)
		_, lat, ok, has := s.st.ServiceLast(sv.ID)
		rate, rateHas := serviceUptime24(hist, now)
		st := s.st.ServiceStats(sv.ID, now-3600)
		sp := svcPub{Name: sv.Name, Uptime: round1(rate), Has: has,
			LossPct: round1(st.LossPct), JitterMs: round1(st.JitterMs)}
		if has {
			sp.OK = ok
			sp.Latency = lat
		}
		if rateHas {
			sp.FailRate = round1(100 - rate)
		}
		if pts, _, _ := serviceLatencyPoints(hist, now, 24); len(pts) > 0 {
			sp.LatencySpark = pts
		}
		svcOut = append(svcOut, sp)
	}

	svcDown := 0
	for _, sp := range svcOut {
		if sp.Has && !sp.OK {
			svcDown++
		}
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
	maintAny := maintAll || len(maintNodes) > 0
	summary := s.buildStatusSummary(out, len(svcOut), svcDown, maintAny)

	names := map[string]string{}
	ids := make([]string, 0, len(out))
	for _, n := range s.st.Nodes() {
		names[n.ID] = n.Name
		ids = append(ids, n.ID)
	}

	resp := map[string]any{
		"site": cfg.Title,
		// 区块顺序由管理员配置，访客端按此渲染（缺失时前端回退默认顺序）
		"sections": cfg.Sections,
		"nodes":    out,
		"online":   counts[model.NodeStateOnline],
		"stale":    counts[model.NodeStateStale],
		"offline":  counts[model.NodeStateOffline],
		"noData":   counts[model.NodeStateNoData],
		"total":    len(out),
		"svcDown":  svcDown,
	}
	if cfg.Blocks.Services {
		resp["services"] = svcOut
	}
	if cfg.Blocks.Summary {
		resp["summary"] = summary
	}
	if cfg.Blocks.History {
		resp["historyDays"] = cfg.HistoryDays
		resp["history"] = s.fleetDailyUptime(ids, cfg.HistoryDays, now)
	}
	if cfg.Blocks.Incidents {
		resp["incidents"] = s.buildTimeline(now-int64(cfg.HistoryDays)*86400, 60,
			func(id string) string { return names[id] })
	}
	if cfg.Announcement.Enabled {
		resp["announcement"] = cfg.Announcement
	}
	if cfg.Blocks.Map {
		counts := map[string]int{}
		for _, p := range out {
			if p.Country != "" {
				counts[p.Country]++
			}
		}
		resp["map"] = counts
	}
	if cfg.Blocks.Activity {
		// 活动流只下发公开可见的事件；资产到期等运营信息不进入公开页。
		feed := make([]map[string]any, 0, 20)
		for _, ev := range s.st.Events(40) {
			switch ev.Kind {
			case "node_online", "node_offline", "alert_opened", "alert_resolved":
			default:
				continue
			}
			feed = append(feed, map[string]any{
				"ts": ev.TS, "kind": ev.Kind, "level": ev.Level, "text": ev.Text,
			})
			if len(feed) >= 20 {
				break
			}
		}
		resp["activity"] = feed
	}
	resp["maintenance"] = maintenanceBanner(maintGlobal, maintNodes)
	resp["visibility"] = vis
	resp["blocks"] = cfg.Blocks
	resp["generatedAt"] = now
	writeJSON(w, 200, resp)
}

// maintenanceBanner 生成整站级别的维护提示。
// 全局窗口优先展示；只有部分节点在维护时说明受影响数量，
// 避免访客看到"维护中"却不知道是全部还是一台。
// 只使用 MaintenanceAt 返回的生效窗口，避免把尚未开始/已经结束的标题展示出来。
func maintenanceBanner(global *model.Maintenance, nodes map[string]*model.Maintenance) map[string]any {
	if global == nil && len(nodes) == 0 {
		return nil
	}
	title := "计划维护"
	if global != nil && global.Title != "" {
		title = global.Title
	} else {
		for _, m := range nodes {
			if m != nil && m.Title != "" {
				title = m.Title
				break
			}
		}
	}
	out := map[string]any{"title": title, "global": global != nil, "count": len(nodes)}
	if global != nil {
		out["text"] = "正在进行计划维护，期间可能出现短暂中断。"
	} else {
		out["text"] = "部分节点正在进行计划维护。"
	}
	return out
}

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
	Token         string `json:"token"`
	AgentOutdated bool   `json:"agentOutdated,omitempty"`
}

// versionOutdated 判断 agent 是否落后于 server。
// 任一侧不是可解析的版本（如 dev）时不做提示，避免开发构建误报。
func versionOutdated(agentVersion, serverVersion string) bool {
	return selfupdate.ValidVersion(agentVersion) &&
		selfupdate.ValidVersion(serverVersion) &&
		selfupdate.IsNewer(serverVersion, agentVersion)
}

func agentVersionOutdated(agentVersion string) bool {
	return versionOutdated(agentVersion, version.Version)
}

func adminNode(n *model.Node) adminNodeView {
	return adminNodeView{
		Node:          *n,
		Token:         n.Token,
		AgentOutdated: agentVersionOutdated(n.AgentVersion),
	}
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
	n.Country = strings.ToUpper(strings.TrimSpace(n.Country))
	if n.Name == "" {
		return "name is required"
	}
	if n.Country != "" {
		if len(n.Country) != 2 {
			return "country 必须是两位 ISO 国家代码（如 US / JP / DE）"
		}
		for _, r := range n.Country {
			if r < 'A' || r > 'Z' {
				return "country 必须是两位字母国家代码"
			}
		}
	}
	if n.TrafficQuotaGB < 0 || n.TrafficQuotaGB > 1000000 {
		return "trafficQuotaGb 必须在 0–1000000 之间"
	}
	if n.TrafficResetDay < 0 || n.TrafficResetDay > 28 {
		return "trafficResetDay 必须在 0–28 之间"
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
			out = append(out, adminNode(n))
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
		// 版本只由 Agent 上报头写入，不接受台账 API 伪造。
		in.AgentVersion = ""
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
		writeJSON(w, 201, adminNode(&in))

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
	// ID 直接来自 URL 剩余部分：长度与字符集不设限时，超长 ID 会进入
	// 日志、错误响应与走查逻辑（且每次都要复制一遍整段路径）。
	if id == "" {
		fail(w, 400, "missing node id")
		return
	}
	// 复用 statusadmin.go 的字符集校验：节点 ID 同样会进入日志与 DOM。
	if !validRecordID(id) {
		fail(w, 400, "invalid node id")
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
		writeJSON(w, 200, adminNode(n))
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
		updated.Country = in.Country
		updated.Favorite = in.Favorite
		updated.TrafficQuotaGB = in.TrafficQuotaGB
		updated.TrafficResetDay = in.TrafficResetDay
		// ID 与 Token 来自原节点，避免一次台账编辑静默换掉 Agent 凭据。
		s.st.UpsertNode(&updated)
		s.publishChange()
		writeJSON(w, 200, adminNode(&updated))
	case http.MethodDelete:
		if _, ok := s.st.Node(id); !ok {
			fail(w, 404, "node not found")
			return
		}
		s.st.DeleteNode(id)
		// 删除也要唤醒面板：否则要等下一次上报周期（最长 30 秒）才消失。
		s.publishChange()
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
		if msg := s.normalizeService(&in); msg != "" {
			fail(w, 400, msg)
			return
		}
		if in.ID == "" {
			in.ID = store.NewID("s")
		}
		// 客户端自带 ID 时按「新建」处理：已存在必须报 409，
		// 不能让一次复制粘贴的 POST 静默覆盖线上服务配置。
		if req.ID != "" && !s.st.CreateService(&in) {
			fail(w, 409, "service id already exists")
			return
		}
		if req.ID == "" {
			s.st.UpsertService(&in)
		}
		writeJSON(w, 201, in)

	default:
		fail(w, 405, "method not allowed")
	}
}

// normalizeService 统一校验并规整服务定义，POST 与 PUT 共用。
func (s *Server) normalizeService(in *model.Service) string {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return "name is required"
	}
	typ := strings.ToLower(strings.TrimSpace(in.Type))
	if typ != "http" && typ != "https" && typ != "tcp" && typ != "dns" {
		return "type 必须是 http / https / tcp / dns"
	}
	in.Type = typ
	in.Target = strings.TrimSpace(in.Target)
	if in.Target == "" {
		return "target is required"
	}
	if in.Interval <= 0 {
		in.Interval = 60
	}
	if in.Interval < 10 {
		return "interval 不能小于 10 秒"
	}
	if in.Interval > 86400 {
		return "interval 不能超过 86400 秒"
	}
	if s.runner != nil {
		if err := s.runner.ValidateService(*in); err != nil {
			return err.Error()
		}
	}
	return ""
}

func (s *Server) handleAdminServiceByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/admin/services/")
	if rest == "" || strings.HasPrefix(rest, "/") {
		fail(w, 400, "missing service id")
		return
	}
	// POST /api/admin/services/{id}/test 立即执行一次探测，不写历史。
	// 结果只用于后台「检测」按钮核对目标与证书状态。
	if id, ok := strings.CutSuffix(rest, "/test"); ok {
		if r.Method != http.MethodPost {
			fail(w, 405, "method not allowed")
			return
		}
		if id == "" || strings.Contains(id, "/") {
			fail(w, 404, "service not found")
			return
		}
		sv, found := s.st.Service(id)
		if !found {
			fail(w, 404, "service not found")
			return
		}
		if s.runner == nil {
			fail(w, 503, "探测执行器未启用")
			return
		}
		writeJSON(w, 200, s.runner.Do(*sv))
		return
	}
	id := rest
	switch r.Method {
	case http.MethodPut:
		old, ok := s.st.Service(id)
		if !ok {
			fail(w, 404, "service not found")
			return
		}
		body := http.MaxBytesReader(w, r.Body, 16<<10)
		var req struct {
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
			ID: id, NodeID: req.NodeID, Name: req.Name,
			Type: req.Type, Target: req.Target, Interval: req.Interval,
			// 未提供 enabled 时保持原状态，避免一次编辑把已停用的探测悄悄启用。
			Enabled: old.Enabled,
		}
		if req.Enabled != nil {
			in.Enabled = *req.Enabled
		}
		if msg := s.normalizeService(&in); msg != "" {
			fail(w, 400, msg)
			return
		}
		s.st.UpsertService(&in)
		writeJSON(w, 200, in)

	case http.MethodDelete:
		active := s.st.ActiveAlertsByService(id)
		if !s.st.DeleteService(id) {
			fail(w, 404, "service not found")
			return
		}
		if s.engine != nil {
			s.engine.RecoverClosed(active, "服务已删除")
		}
		writeJSON(w, 200, map[string]any{"deleted": id})

	default:
		fail(w, 405, "method not allowed")
	}
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
			"version":   version.Version,
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
		// 读取旧配置、还原脱敏占位符、落盘与生效必须整体串行：
		// 否则两个并发保存会各自基于同一份旧配置还原密钥，后写入者
		// 把前一次刚保存的新密钥覆盖回旧值。
		s.notifySaveMu.Lock()
		// 旧配置损坏时按「首次配置」处理，否则上面那条 500/409
		// 会把损坏状态永久固化，再也无法覆盖。
		old, err := notify.Parse(s.st.GetSetting("notify"))
		if err != nil {
			log.Printf("已保存的通知配置损坏，按首次配置覆盖: %v", err)
			old = nil
		}
		in.MergeSecrets(old)
		raw, merr := json.Marshal(&in)
		if merr == nil {
			s.st.SetSetting("notify", string(raw))
			s.applyNotifyConfig(&in)
		}
		s.notifySaveMu.Unlock()
		if merr != nil {
			fail(w, 500, "序列化失败")
			return
		}
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
		// 测试通知必须带上已保存的模板，否则用户无法在面板里验证
		// 自定义标题/正文是否符合预期。
		TitleFormat: cfg.TitleFormat,
		BodyFormat:  cfg.BodyFormat,
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
	// 这里必须再次检查并与写入放在同一临界区：上面的快速失败不占配额，
	// 两个并发请求可能同时通过快速检查，只有这里能保证只放行一个。
	s.testMu.Lock()
	if wait := 3*time.Second - time.Since(s.lastTest); wait > 0 && !s.lastTest.IsZero() {
		s.testMu.Unlock()
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		fail(w, 429, "测试通知过于频繁，请稍后再试")
		return
	}
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
	services := s.st.Services()
	type serviceOption struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		NodeID string `json:"nodeId"`
	}
	svcOpts := make([]serviceOption, 0, len(services))
	for _, sv := range services {
		svcOpts = append(svcOpts, serviceOption{ID: sv.ID, Name: sv.Name, NodeID: sv.NodeID})
	}
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
			{Value: "swap", Label: "Swap 使用率", Unit: "%", Op: "gt", Min: 1, Max: 100, Step: 1,
				Hint: "Swap 持续占用通常意味着内存不足"},
			{Value: "load", Label: "1 分钟负载", Unit: "", Op: "gt", Min: 0.1, Max: 10000, Step: 0.1,
				Hint: "建议设为 CPU 核心数的 1–2 倍"},
			{Value: "load5", Label: "5 分钟负载", Unit: "", Op: "gt", Min: 0.1, Max: 10000, Step: 0.1,
				Hint: "用于识别持续性负载"},
			{Value: "load15", Label: "15 分钟负载", Unit: "", Op: "gt", Min: 0.1, Max: 10000, Step: 0.1,
				Hint: "用于识别长期过载"},
			{Value: "tcp_conn", Label: "TCP 连接数", Unit: "个", Op: "gt", Min: 1, Max: 1000000, Step: 1,
				Hint: "可发现连接泄漏或异常扫描"},
			{Value: "procs", Label: "进程数", Unit: "个", Op: "gt", Min: 1, Max: 1000000, Step: 1,
				Hint: "进程暴涨可能是 fork 风暴"},
			{Value: "traffic_quota", Label: "流量配额使用率", Unit: "%", Op: "gt", Min: 1, Max: 1000, Step: 1,
				Hint: "按节点设置的月度配额计算；常用阈值 90–100"},
			{Value: "service_down", Label: "服务探测失败", Unit: "", Op: "gt", NoVal: true,
				Hint: "HTTP/TCP/DNS 探测失败即触发；阈值固定为 1"},
			{Value: "latency", Label: "服务延迟", Unit: "ms", Op: "gt", Min: 1, Max: 600000, Step: 10,
				Hint: "作用于服务探测；常用阈值 1000–3000"},
			{Value: "loss", Label: "服务丢包率", Unit: "%", Op: "gt", Min: 1, Max: 100, Step: 1,
				Hint: "最近 1 小时探测失败占比"},
			{Value: "jitter", Label: "服务抖动", Unit: "ms", Op: "gt", Min: 1, Max: 600000, Step: 1,
				Hint: "最近 1 小时相邻成功探测的延迟差均值"},
			{Value: "cert_days", Label: "证书剩余天数", Unit: "天", Op: "lt", Min: 1, Max: 3650, Step: 1,
				Hint: "仅作用于 HTTPS 服务；常用阈值 7–30"},
		},
		"services": svcOpts,
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
			ID         string   `json:"id"`
			Name       string   `json:"name"`
			Metric     string   `json:"metric"`
			Op         string   `json:"op"`
			Threshold  *float64 `json:"threshold"`
			Duration   int      `json:"duration"`
			Severity   string   `json:"severity"`
			Enabled    *bool    `json:"enabled"`
			NodeIDs    []string `json:"nodeIds"`
			Groups     []string `json:"groups"`
			ServiceIDs []string `json:"serviceIds"`
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
			fail(w, 400, "metric 无效")
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

		// offline / service_down 规则不需要阈值；其余指标必须提供且在合理区间
		threshold := 0.0
		if req.Metric != "offline" && req.Metric != "service_down" {
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
			ID:         req.ID,
			Name:       strings.TrimSpace(req.Name),
			Metric:     req.Metric,
			Op:         normalizeOp(req.Op, req.Metric),
			Threshold:  threshold,
			Duration:   duration,
			Severity:   severity,
			Enabled:    true,
			NodeIDs:    normalizeScope(req.NodeIDs),
			Groups:     normalizeScope(req.Groups),
			ServiceIDs: normalizeScope(req.ServiceIDs),
		}
		if req.Enabled != nil {
			rule.Enabled = *req.Enabled
		}
		if rule.ID == "" {
			rule.ID = store.NewID("r")
		} else if _, exists := s.ruleByID(rule.ID); exists {
			// 与节点/服务的创建路径保持一致：带 ID 的 POST 只允许新建。
			// 静默覆盖会让"创建"实际变成"改配置"，而调用方以为多了一条规则。
			fail(w, 409, "规则 ID 已存在，更新请使用 PUT /api/admin/rules/"+rule.ID)
			return
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
			Name       string    `json:"name"`
			Metric     string    `json:"metric"`
			Op         string    `json:"op"`
			Threshold  *float64  `json:"threshold"`
			Duration   *int      `json:"duration"`
			Severity   string    `json:"severity"`
			Enabled    *bool     `json:"enabled"`
			NodeIDs    *[]string `json:"nodeIds"`
			Groups     *[]string `json:"groups"`
			ServiceIDs *[]string `json:"serviceIds"`
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
		} else if req.Metric != "" && req.Metric != existing.Metric {
			// 换指标时必须显式给方向：cpu 用 gt、cert_days 用 lt，
			// 沿用旧指标的 op 会得到一个语义相反的规则（例如"证书剩余天数 > 7 告警"）。
			fail(w, 400, "切换 metric 时必须同时提供 op（gt / lt）")
			return
		}
		if req.Threshold != nil {
			// offline 规则的阈值是「失联多少秒」，与 cpu/mem 的量纲无关。
			// 原来这里借道 thresholdRange 拿到 (0,0)，导致前端整体回传规则时
			// 被 400 拒绝；改为按秒数区间单独校验，并归一化为 0。
			if updated.Metric == "offline" || updated.Metric == "service_down" {
				if *req.Threshold != 0 && *req.Threshold != 1 {
					fail(w, 400, "该指标的阈值固定为 1，无需填写")
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
		if req.ServiceIDs != nil {
			updated.ServiceIDs = normalizeScope(*req.ServiceIDs)
		}
		if updated.Metric == "offline" || updated.Metric == "service_down" {
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
	case "offline", "cpu", "mem", "disk", "swap", "load", "load5", "load15",
		"tcp_conn", "procs", "traffic_quota",
		"service_down", "latency", "loss", "jitter", "cert_days":
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
	case "cpu", "mem", "disk", "swap", "loss":
		return 1, 100
	case "load", "load5", "load15":
		return 0.1, 10000
	case "tcp_conn":
		return 1, 1000000
	case "procs":
		return 1, 1000000
	case "latency", "jitter":
		return 1, 600000
	case "cert_days":
		return 1, 3650
	case "traffic_quota":
		return 1, 1000
	case "offline", "service_down":
		return 0, 0
	}
	return 0, 0
}

// normalizeOp 归一化比较符：offline 固定 gt；证书剩余天数默认 lt；其余默认 gt。
func normalizeOp(op, metric string) string {
	if metric == "offline" || metric == "service_down" {
		return "gt"
	}
	if op == "lt" || op == "gt" {
		return op
	}
	if metric == "cert_days" {
		return "lt"
	}
	return "gt"
}

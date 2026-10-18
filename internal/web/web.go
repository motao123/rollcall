// Package web 提供 HTTP API 与内嵌前端资源。
package web

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/motao/rollcall/internal/alert"
	"github.com/motao/rollcall/internal/model"
	"github.com/motao/rollcall/internal/notify"
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
	limiter    *authFailureLimiter
	notifyErr  error

	testMu   sync.Mutex
	lastTest time.Time
}

// Config 是服务端的运行参数。
type Config struct {
	AdminToken string // 管理 API 令牌（为空则不开放管理 API）
	AuthUser   string // Basic Auth 用户名（为空则不启用）
	AuthPass   string
	PanelLink  string // 面板对外地址，拼进通知正文
}

func New(st *store.Store, r *probe.Runner, e *alert.Engine, cfg Config) *Server {
	srv := &Server{st: st, runner: r, engine: e,
		adminToken: cfg.AdminToken, authUser: cfg.AuthUser, authPass: cfg.AuthPass,
		panelLink: cfg.PanelLink, limiter: newAuthFailureLimiter()}
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

// applyNotifyConfig 把通知配置同步到告警引擎。
func (s *Server) applyNotifyConfig(cfg *notify.Config) {
	if s.engine == nil {
		return
	}
	s.engine.SetRepeat(cfg.RepeatInterval)
	s.engine.SetLink(s.panelLink)
	if !cfg.Enabled || len(cfg.EnabledChannels()) == 0 {
		s.engine.SetNotifier(nil)
		return
	}
	snapshot := *cfg
	s.engine.SetNotifier(func(m notify.Message) {
		if !snapshot.ShouldSend(m.Severity) {
			return
		}
		go notify.Send(&snapshot, m) // 异步投递，避免阻塞评估循环
	})
}

// Handler 返回配置好的路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/overview", s.handleOverview)
	mux.HandleFunc("/api/nodes", s.handleNodes)
	mux.HandleFunc("/api/nodes/", s.handleNodeDetail)
	mux.HandleFunc("/api/services", s.handleServices)
	mux.HandleFunc("/api/alerts", s.handleAlerts)
	mux.HandleFunc("/api/rules", s.handleRules)
	mux.HandleFunc("/api/assets", s.handleAssets)
	mux.HandleFunc("/api/report", s.handleReport)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/status", s.handleStatusPage)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})

	// 管理 API（需 X-Admin-Token）
	mux.HandleFunc("/api/admin/nodes", s.handleAdminNodes)
	mux.HandleFunc("/api/admin/nodes/", s.handleAdminNodeByID)
	mux.HandleFunc("/api/admin/services", s.handleAdminServices)
	mux.HandleFunc("/api/admin/services/", s.handleAdminServiceByID)
	mux.HandleFunc("/api/admin/settings", s.handleAdminSettings)
	mux.HandleFunc("/api/admin/test-notify", s.handleTestNotify)
	mux.HandleFunc("/api/admin/rules", s.handleAdminRules)
	mux.HandleFunc("/api/admin/rules/", s.handleAdminRuleByID)
	mux.HandleFunc("/api/admin/rule-meta", s.handleRuleMeta)
	mux.HandleFunc("/api/export/", s.handleExport)
	mux.HandleFunc("/metrics", s.handleMetrics)

	// 静态资源
	sub, err := fs.Sub(assets, "dist")
	if err != nil {
		panic(err)
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))

	return withCommon(s.withAuthGuard(withAuth(mux, s)))
}

// withAuth 对浏览器访问启用 Basic Auth；Agent 上报走自己的 token，不受影响。
func withAuth(h http.Handler, s *Server) http.Handler {
	if s.authUser == "" {
		return h
	}
	// 用户名已配置但密码为空属于配置错误，必须整体拒绝而不是放行空密码
	if s.authPass == "" {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "server auth misconfigured", http.StatusInternalServerError)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Agent 上报用 X-Node-Token 自行鉴权；/status 与 /api/status 为公开页，
		// /status.js 是公开页依赖的静态脚本，必须一并放行，否则页面会卡在骨架屏。
		switch r.URL.Path {
		case "/api/report", "/healthz", "/status", "/api/status", "/status.js":
			h.ServeHTTP(w, r)
			return
		}
		u, p, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(u), []byte(s.authUser)) != 1 ||
			subtle.ConstantTimeCompare([]byte(p), []byte(s.authPass)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="Rollcall", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
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
	s.st.AddMetrics(node.ID, rep.Host, rep.Metrics)

	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- 视图 ----------

func (s *Server) buildNodeViews() []model.NodeView {
	nodes := s.st.Nodes()
	alertByNode := s.st.ActiveAlertsByNode()
	now := time.Now().Unix()
	out := make([]model.NodeView, 0, len(nodes))

	for _, n := range nodes {
		m, ts, has := s.st.Latest(n.ID)
		online := has && now-ts <= store.OnlineWindow

		// 迷你曲线：最近 30 个点的 CPU
		series := s.st.Series(n.ID, now-1200)
		if len(series) > 30 {
			series = series[len(series)-30:]
		}
		spark := make([]model.Point, 0, len(series))
		for _, p := range series {
			spark = append(spark, model.Point{T: p.TS, V: p.CPUPct})
		}

		// 视图不携带上报令牌：即使未来有非 JSON 序列化路径也无法泄露。
		// adminNodeView 会显式附加自己需要的 token 字段。
		cp := *n
		cp.Token = ""
		out = append(out, model.NodeView{
			Node:     cp,
			Host:     s.st.Host(n.ID),
			Latest:   m,
			Online:   online,
			LastSeen: ts,
			Alerts:   alertByNode[n.ID],
			Spark:    spark,
		})
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
		if v.Online {
			ov.Online++
			cpuSum += v.Latest.CPUPct
			memSum += pctF(v.Latest.MemUsed, v.Latest.MemTotal)
			ov.TrafficIn += v.Latest.NetRx
			ov.TrafficOut += v.Latest.NetTx
			cnt++
		} else {
			ov.Offline++
		}
		// 成本只取决于价格，与是否登记到期日无关；
		// 否则未填到期日的资产会在总览里凭空消失。
		ov.MonthlyCost += monthly(v.Price, v.Currency)
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

	// /api/nodes/{id}/series?range=1h
	if len(parts) > 1 && parts[1] == "series" {
		rng := r.URL.Query().Get("range")
		window := int64(3600)
		switch rng {
		case "6h":
			window = 6 * 3600
		case "24h":
			window = 24 * 3600
		}
		since := time.Now().Unix() - window
		pts := s.st.Series(id, since)
		writeJSON(w, 200, map[string]any{"node": n, "range": rng, "points": pts})
		return
	}

	m, ts, _ := s.st.Latest(id)
	writeJSON(w, 200, map[string]any{
		"node":     n,
		"host":     s.st.Host(id),
		"latest":   m,
		"lastSeen": ts,
		"online":   time.Now().Unix()-ts <= store.OnlineWindow,
	})
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
		up, tot := 0, 0
		for _, h := range hist {
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
	writeJSON(w, 200, map[string]any{
		"active": s.st.ActiveAlertCount(),
		"items":  s.st.Alerts(200),
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
		mv := monthly(n.Price, n.Currency)
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
			Name: v.Name, Group: v.Group, Online: v.Online,
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

	online := 0
	for _, p := range out {
		if p.Online {
			online++
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
		"online":      online,
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

func monthly(price float64, cur string) float64 {
	// 简化：按月成本 = 年付价格 / 12；CNY 折算为 USD 仅用于聚合展示
	if cur == "CNY" {
		return price / 12 / 7.2
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

func (s *Server) handleAdminNodes(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		fail(w, 401, "invalid admin token")
		return
	}

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
		if strings.TrimSpace(in.Name) == "" {
			fail(w, 400, "name is required")
			return
		}
		if in.ID == "" {
			in.ID = store.NewID("n")
			// 全新节点：服务端生成上报令牌，不接受客户端指定，避免弱令牌
			in.Token = store.NewToken()
		} else if old, ok := s.st.Node(in.ID); ok {
			// 更新已存在节点：**保留原令牌**，否则会静默轮换导致该节点 Agent 立即 401。
			// 需要轮换时应显式走独立流程，不应由一次普通的台账更新触发。
			in.Token = old.Token
		} else {
			in.Token = store.NewToken()
		}
		if in.Currency == "" {
			in.Currency = "USD"
		}
		s.st.UpsertNode(&in)
		writeJSON(w, 201, adminNodeView{Node: in, Token: in.Token})

	default:
		fail(w, 405, "method not allowed")
	}
}

func (s *Server) handleAdminNodeByID(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		fail(w, 401, "invalid admin token")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/admin/nodes/")
	if id == "" {
		fail(w, 400, "missing node id")
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

// ---------- 服务探测管理 API ----------

func (s *Server) handleAdminServices(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		fail(w, 401, "invalid admin token")
		return
	}
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
	if !s.adminOK(r) {
		fail(w, 401, "invalid admin token")
		return
	}
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
	if !s.adminOK(r) {
		fail(w, 401, "invalid admin token")
		return
	}
	switch r.Method {
	case http.MethodGet:
		cfg, err := notify.Parse(s.st.GetSetting("notify"))
		if err != nil {
			fail(w, 500, "配置解析失败: "+err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{
			"notify":    cfg.Redacted(),
			"channels":  cfg.EnabledChannels(),
			"defs":      notify.Defs(),
			"panelLink": s.panelLink,
		})

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
		// 还原前端回传的脱敏占位符
		old, err := notify.Parse(s.st.GetSetting("notify"))
		if err != nil {
			fail(w, 409, "已保存的通知配置已损坏，请重新填写所有密钥后再保存")
			return
		}
		in.MergeSecrets(old)

		raw, err := json.Marshal(&in)
		if err != nil {
			fail(w, 500, "序列化失败")
			return
		}
		s.st.SetSetting("notify", string(raw))
		s.applyNotifyConfig(&in)
		writeJSON(w, 200, map[string]any{"saved": true, "channels": in.EnabledChannels()})

	default:
		fail(w, 405, "method not allowed")
	}
}

// handleTestNotify 发一条测试通知，验证通道配置是否正确。
func (s *Server) handleTestNotify(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		fail(w, 401, "invalid admin token")
		return
	}
	if r.Method != http.MethodPost {
		fail(w, 405, "method not allowed")
		return
	}
	// 测试通知会真实外发，限制频率避免被拿来当扫描器或刷屏。
	s.testMu.Lock()
	if wait := 3*time.Second - time.Since(s.lastTest); wait > 0 && !s.lastTest.IsZero() {
		s.testMu.Unlock()
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		fail(w, 429, "测试通知过于频繁，请稍后再试")
		return
	}
	s.lastTest = time.Now()
	s.testMu.Unlock()

	// 可选：只测某个通道（body 里带 channelId）
	var req struct {
		ChannelID string `json:"channelId"`
	}
	body := http.MaxBytesReader(w, r.Body, 4<<10)
	_ = json.NewDecoder(body).Decode(&req)

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
	if !s.adminOK(r) {
		fail(w, 401, "invalid admin token")
		return
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
	if !s.adminOK(r) {
		fail(w, 401, "invalid admin token")
		return
	}
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
	if !s.adminOK(r) {
		fail(w, 401, "invalid admin token")
		return
	}
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
			Name      string   `json:"name"`
			Metric    string   `json:"metric"`
			Op        string   `json:"op"`
			Threshold *float64 `json:"threshold"`
			Duration  *int     `json:"duration"`
			Severity  string   `json:"severity"`
			Enabled   *bool    `json:"enabled"`
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
			lo, hi := thresholdRange(updated.Metric)
			if *req.Threshold < lo || *req.Threshold > hi {
				fail(w, 400, fmt.Sprintf("threshold 必须在 %v – %v 之间", lo, hi))
				return
			}
			updated.Threshold = *req.Threshold
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
		s.st.DeleteRule(id)
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

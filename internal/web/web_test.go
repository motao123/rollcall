package web

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/motao123/rollcall/internal/alert"
	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/notify"
	"github.com/motao123/rollcall/internal/store"
	"github.com/motao123/rollcall/internal/version"
)

// TestUptime24UsesDurationNotSampleCount 是回归测试：
// 旧实现把「收到的采样点数」同时当分子分母，只要节点还在上报就恒为 100%。
func TestUptime24UsesDurationNotSampleCount(t *testing.T) {
	const now int64 = 1_700_000_000

	var series []model.Metrics
	for ts := now - 3600; ts <= now; ts += 30 {
		series = append(series, model.Metrics{TS: ts})
	}
	if up, _ := uptime24(series, now, true); up != 100 {
		t.Errorf("连续上报应得 100%%，实际 %.1f", up)
	}

	// 10 分钟前一个点，然后中断到当前：只有 OnlineWindow(120s) 算在线
	sparse := []model.Metrics{{TS: now - 600}, {TS: now}}
	up, _ := uptime24(sparse, now, true)
	if up > 25 || up < 15 {
		t.Errorf("中断后的可用率应约 20%%，实际 %.1f", up)
	}

	if up, _ := uptime24(nil, now, true); up != 100 {
		t.Errorf("无历史但当前在线应显示 100%%，实际 %.1f", up)
	}
	if up, _ := uptime24(nil, now, false); up != 0 {
		t.Errorf("无历史且离线应为 0%%，实际 %.1f", up)
	}
}

func TestServiceUptime24FiltersOldSamples(t *testing.T) {
	const now int64 = 1_700_000_000
	var hist [][3]int64
	// 25 小时前的失败样本不应计入 24h
	hist = append(hist, [3]int64{now - 90000, 0, 0})
	for i := int64(0); i < 10; i++ {
		hist = append(hist, [3]int64{now - i*60, 10, 1})
	}
	up, has := serviceUptime24(hist, now)
	if !has || up != 100 {
		t.Errorf("24h 可用率应忽略陈旧样本: up=%.1f has=%v", up, has)
	}
	if _, has := serviceUptime24(nil, now); has {
		t.Error("无探测数据时 has 应为 false")
	}
}

// TestAdminAuthRateLimit 验证连续 401 后返回 429，避免无限爆破。
func TestAdminAuthRateLimit(t *testing.T) {
	st := store.New(t.TempDir())
	srv := New(st, nil, nil, Config{AdminToken: "test-admin-token-1234567890"})

	var last *httptest.ResponseRecorder
	for i := 0; i < 11; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/nodes", nil)
		req.Header.Set("X-Admin-Token", "wrong")
		last = httptest.NewRecorder()
		srv.Handler().ServeHTTP(last, req)
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("连续失败后应返回 429，实际 %d", last.Code)
	}
	if last.Header().Get("Retry-After") == "" {
		t.Error("429 应带 Retry-After")
	}
}

func TestRuleMetaRequiresAdminToken(t *testing.T) {
	st := store.New(t.TempDir())
	srv := New(st, nil, nil, Config{AdminToken: "test-admin-token-1234567890"})

	req := httptest.NewRequest(http.MethodGet, "/api/admin/rule-meta", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("无令牌访问 rule-meta 应 401，实际 %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/admin/rule-meta", nil)
	req.Header.Set("X-Admin-Token", "test-admin-token-1234567890")
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("带令牌访问 rule-meta 应 200，实际 %d", w.Code)
	}
}

func TestEmptyBasicAuthPasswordFailsClosed(t *testing.T) {
	st := store.New(t.TempDir())
	srv := New(st, nil, nil, Config{AdminToken: "test-admin-token-1234567890", AuthUser: "admin"})
	req := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("空密码配置应整体拒绝，实际 %d", w.Code)
	}
}

func TestCSVSafeBlocksFormulaInjection(t *testing.T) {
	cases := []struct{ in, want string }{
		{"=cmd|'/c calc'!A1", "'=cmd|'/c calc'!A1"},
		{"+1234", "'+1234"},
		{"-2+3", "'-2+3"},
		{"@SUM(A1)", "'@SUM(A1)"},
		{"\t=1+1", "'\t=1+1"},
		{"\r=1+1", "'\r=1+1"},
		// 正常数据零影响
		{"腾讯云-sg", "腾讯云-sg"},
		{"node-01", "node-01"},
		{"Ubuntu 22.04", "Ubuntu 22.04"},
		{"", ""},
	}
	for _, c := range cases {
		if got := csvSafe(c.in); got != c.want {
			t.Errorf("csvSafe(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

func TestSanitizeStripsHeaderInjection(t *testing.T) {
	cases := []struct{ in, want string }{
		{"正常节点名", "正常节点名"},
		{"a/b\\c:d", "a-b-c-d"},
		// CR/LF 必须被移除，否则可注入 Content-Disposition 之外的响应头
		// （冒号与空格也会被 Replacer 换成 -，故此处是 "--"）
		{"evil\r\nX-Injected: yes", "evilX-Injected--yes"},
		{"tab\tname", "tab-name"},
		{"ctrl\x00\x1fend", "ctrlend"},
	}
	for _, c := range cases {
		got := safeFileName(c.in)
		if got != c.want {
			t.Errorf("safeFileName(%q) = %q, 期望 %q", c.in, got, c.want)
		}
		// 无论输入如何，输出绝不能含 CR/LF
		if bytes.ContainsAny([]byte(got), "\r\n") {
			t.Errorf("safeFileName(%q) 仍含 CR/LF: %q", c.in, got)
		}
	}
}

// TestNodeUpdatePreservesToken 是回归测试。
//
// 曾经的缺陷：POST /api/admin/nodes 无条件 in.Token = store.NewToken()，
// 于是带着已存在 ID 更新台账会**静默轮换令牌**，该节点 Agent 立即开始 401。
// 现在 POST 只负责创建（重复 ID 返回 409），更新必须走 PUT，且不得触碰令牌。
func TestNodeUpdatePreservesToken(t *testing.T) {
	st := store.New(t.TempDir())
	const adminTok = "test-admin-token-1234567890"

	// 预置一个已有令牌的节点
	n := &model.Node{ID: "n1", Name: "节点一", Provider: "old", Token: "original-token"}
	st.UpsertNode(n)

	srv := New(st, nil, nil, Config{AdminToken: adminTok})
	h := srv.Handler()

	// 用同一 ID 走 POST 必须被拒绝，而不是当成「更新」静默改写台账。
	body, _ := json.Marshal(model.Node{ID: "n1", Name: "节点一改", Provider: "new"})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/nodes", bytes.NewReader(body))
	req.Header.Set("X-Admin-Token", adminTok)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("重复 ID 的 POST 应返回 409，实际 %d: %s", w.Code, w.Body.String())
	}

	// 更新走 PUT：台账字段生效，令牌保持不变。
	req = httptest.NewRequest(http.MethodPut, "/api/admin/nodes/n1", bytes.NewReader(body))
	req.Header.Set("X-Admin-Token", adminTok)
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("更新节点返回 %d: %s", w.Code, w.Body.String())
	}
	var got adminNodeView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if got.Token != "original-token" {
		t.Errorf("令牌被轮换: got %q, 期望保留 %q", got.Token, "original-token")
	}
	if got.Name != "节点一改" || got.Provider != "new" {
		t.Errorf("台账字段未更新: %+v", got.Node)
	}
}

// TestNewNodeGetsToken 确认全新节点仍会生成令牌。
func TestNewNodeGetsToken(t *testing.T) {
	st := store.New(t.TempDir())
	const adminTok = "test-admin-token-1234567890"
	srv := New(st, nil, nil, Config{AdminToken: adminTok})

	body, _ := json.Marshal(model.Node{Name: "新节点"})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/nodes", bytes.NewReader(body))
	req.Header.Set("X-Admin-Token", adminTok)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != 201 {
		t.Fatalf("创建节点返回 %d", w.Code)
	}
	var got adminNodeView
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Token) < 32 {
		t.Errorf("新节点未生成有效令牌: %q", got.Token)
	}
	if got.ID == "" {
		t.Error("新节点未生成 ID")
	}
}

// TestAssetsExportEscapesHostOS 验证 Agent 自报的恶意 os 字段被中和。
func TestAssetsExportEscapesHostOS(t *testing.T) {
	st := store.New(t.TempDir())
	const adminTok = "test-admin-token-1234567890"

	st.UpsertNode(&model.Node{ID: "n1", Name: "节点一"})
	// 模拟被攻破的 Agent 自报恶意 os
	st.AddMetrics("n1", model.HostInfo{Hostname: "h", OS: "=cmd|'/c calc'!A1"}, model.Metrics{TS: 1000})

	srv := New(st, nil, nil, Config{AdminToken: adminTok})
	req := httptest.NewRequest(http.MethodGet, "/api/export/assets.csv", nil)
	req.Header.Set("X-Admin-Token", adminTok)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("导出返回 %d", w.Code)
	}
	out := w.Body.String()
	// 恶意公式必须以单引号前缀出现，不能裸 =cmd 开头
	if bytes.Contains([]byte(out), []byte("\n=cmd")) ||
		bytes.Contains([]byte(out), []byte(",=cmd")) {
		t.Errorf("CSV 公式注入未被中和:\n%s", out)
	}
	if !bytes.Contains([]byte(out), []byte("'=cmd")) {
		t.Errorf("未见转义后的安全形式:\n%s", out)
	}
}

// TestDaysUntilUsesLocalCalendar 固定“本地日历日”语义：
// 不能用 UTC 解析 + 时间差截断，否则东八区早上会把“今天到期”算成 -1。
func TestDaysUntilUsesLocalCalendar(t *testing.T) {
	now := time.Now()
	f := func(d time.Time) string { return d.Format("2006-01-02") }
	cases := []struct {
		date string
		want int
	}{
		{f(now), 0},
		{f(now.AddDate(0, 0, 1)), 1},
		{f(now.AddDate(0, 0, 30)), 30},
		{f(now.AddDate(0, 0, -1)), -1},
	}
	for _, c := range cases {
		got, ok := daysUntil(c.date)
		if !ok || got != c.want {
			t.Errorf("daysUntil(%s) = %d,%v，期望 %d,true", c.date, got, ok, c.want)
		}
	}
	if _, ok := daysUntil("2026-13-99"); ok {
		t.Error("非法日期应返回 ok=false")
	}
	if _, ok := daysUntil(""); ok {
		t.Error("空日期应返回 ok=false")
	}
}

// TestPublicNodeViewDoesNotCarryToken 防止视图层携带上报令牌。
//
// 视图不再内嵌 model.Node，令牌字段在类型层面就不存在 —— 即使走非 JSON
// 序列化路径（模板、%+v、未来的 msgpack）也不可能泄露。
func TestPublicNodeViewDoesNotCarryToken(t *testing.T) {
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "节点一", Token: "secret-token"})
	srv := New(st, nil, nil, Config{})
	views := srv.buildNodeViews()
	if len(views) != 1 {
		t.Fatalf("视图数 = %d，期望 1", len(views))
	}
	raw, err := json.Marshal(views)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if strings.Contains(string(raw), "secret-token") {
		t.Fatalf("公开节点视图泄露令牌: %s", raw)
	}
	if strings.Contains(strings.ToLower(string(raw)), "token") {
		t.Fatalf("公开节点视图含 token 字段: %s", raw)
	}
}

// TestDashboardReadAPIsRequireAuth 是回归测试：
// 仪表盘只读接口曾完全匿名，反代未加 Basic Auth 时会直接泄露
// 主机名、内核、服务商、价格、到期日等资产信息。
func TestDashboardReadAPIsRequireAuth(t *testing.T) {
	const adminTok = "test-admin-token-1234567890"
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "节点一", Token: "node-secret"})
	srv := New(st, nil, nil, Config{AdminToken: adminTok})
	h := srv.Handler()

	paths := []string{
		"/api/overview", "/api/nodes", "/api/nodes/n1", "/api/nodes/n1/series",
		"/api/services", "/api/alerts", "/api/rules", "/api/assets",
	}
	for _, p := range paths {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s 无令牌应 401，实际 %d", p, w.Code)
		}
	}
	for _, p := range paths {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		req.Header.Set("X-Admin-Token", adminTok)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("%s 带令牌应 200，实际 %d: %s", p, w.Code, w.Body.String())
		}
	}
}

// TestDashboardReadAPIsAcceptPanelBasic 确认启用 -auth 后，
// 已通过 Basic Auth 的面板用户无需再粘贴管理令牌。
func TestDashboardReadAPIsAcceptPanelBasic(t *testing.T) {
	st := store.New(t.TempDir())
	srv := New(st, nil, nil, Config{
		AdminToken: "test-admin-token-1234567890",
		AuthUser:   "panel",
		AuthPass:   "panel-password-123",
	})
	h := srv.Handler()

	cases := []struct {
		name string
		user string
		pass string
		code int
	}{
		{"no auth", "", "", http.StatusUnauthorized},
		{"wrong password", "panel", "wrong", http.StatusUnauthorized},
		{"panel credentials", "panel", "panel-password-123", http.StatusOK},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/api/nodes", nil)
		if c.user != "" {
			req.SetBasicAuth(c.user, c.pass)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != c.code {
			t.Errorf("%s: 状态码 %d，期望 %d", c.name, w.Code, c.code)
		}
	}

	// 启用 -auth 后，API 客户端仍可只带管理令牌（无需再拼一层 Basic）
	req := httptest.NewRequest(http.MethodGet, "/api/nodes", nil)
	req.Header.Set("X-Admin-Token", "test-admin-token-1234567890")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("仅带管理令牌应 200，实际 %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/admin/nodes", nil)
	req.Header.Set("X-Admin-Token", "test-admin-token-1234567890")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("仅带管理令牌访问管理 API 应 200，实际 %d", w.Code)
	}
}

// TestMetricsAuth 校验 /metrics 在 -auth 下不再被 Basic 前置拦截，
// 同时管理令牌与面板凭据都能抓取。
func TestMetricsAuth(t *testing.T) {
	const adminTok = "test-admin-token-1234567890"
	st := store.New(t.TempDir())

	t.Run("no-basic-config", func(t *testing.T) {
		srv := New(st, nil, nil, Config{AdminToken: adminTok})
		h := srv.Handler()

		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("无凭据应 401，实际 %d", w.Code)
		}

		req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
		req.SetBasicAuth("any", adminTok)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("Basic 密码为管理令牌应 200，实际 %d", w.Code)
		}

		req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
		req.Header.Set("X-Admin-Token", adminTok)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("X-Admin-Token 应 200，实际 %d", w.Code)
		}
	})

	t.Run("with-basic-config", func(t *testing.T) {
		srv := New(st, nil, nil, Config{
			AdminToken: adminTok, AuthUser: "panel", AuthPass: "panel-password-123",
		})
		h := srv.Handler()

		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		req.SetBasicAuth("panel", "panel-password-123")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("面板凭据抓指标应 200，实际 %d", w.Code)
		}

		req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
		req.SetBasicAuth("any", adminTok)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("管理令牌 Basic 抓指标应 200，实际 %d", w.Code)
		}

		// /api/status 仍应匿名可访问
		req = httptest.NewRequest(http.MethodGet, "/api/status", nil)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("/api/status 应保持公开，实际 %d", w.Code)
		}
	})
}

// TestValidateRoutesFailsClosed 校验路由表本身不能出现"未分类"路由。
// 新增接口若漏填级别，Handler 必须在启动时直接 panic，而不是默认放行。
func TestValidateRoutesFailsClosed(t *testing.T) {
	valid := []route{{"/", authPublic, http.NotFoundHandler()}}
	if levels, err := validateRoutes(valid); err != nil || levels["/"] != authPublic {
		t.Fatalf("合法路由表不应报错: levels=%v err=%v", levels, err)
	}

	cases := []struct {
		name   string
		routes []route
	}{
		{"零值级别", []route{
			{"/", authPublic, http.NotFoundHandler()},
			{"/api/x", authUnclassified, http.NotFoundHandler()},
		}},
		{"未知级别", []route{
			{"/", authPublic, http.NotFoundHandler()},
			{"/api/x", authLevel(99), http.NotFoundHandler()},
		}},
		{"空 pattern", []route{
			{"", authPublic, http.NotFoundHandler()},
			{"/", authPublic, http.NotFoundHandler()},
		}},
		{"pattern 不以 / 开头", []route{
			{"api/x", authPublic, http.NotFoundHandler()},
			{"/", authPublic, http.NotFoundHandler()},
		}},
		{"缺 handler", []route{
			{"/api/x", authPublic, nil},
			{"/", authPublic, http.NotFoundHandler()},
		}},
		{"重复 pattern", []route{
			{"/", authPublic, http.NotFoundHandler()},
			{"/", authPublic, http.NotFoundHandler()},
		}},
		{"缺兜底路由", []route{
			{"/api/x", authPublic, http.NotFoundHandler()},
		}},
	}
	for _, c := range cases {
		if _, err := validateRoutes(c.routes); err == nil {
			t.Errorf("%s: 应校验失败，实际通过", c.name)
		}
	}
}

// TestRouteAuthMatrix 对路由表逐项验证鉴权边界：
// 公开端点不要求凭据；登录端点自身公开（handler 内校验）；
// 只读接口接受管理令牌、Basic 或面板会话；管理写操作只认管理令牌或会话 + 同源头；
// /metrics 接受管理令牌或面板凭据。
func TestRouteAuthMatrix(t *testing.T) {
	const adminTok = "test-admin-token-1234567890"
	st := store.New(t.TempDir())
	srv := New(st, nil, nil, Config{
		AdminToken: adminTok, AuthUser: "panel", AuthPass: "panel-password-123",
	})
	h := srv.Handler()

	// 每次请求换一个来源 IP，避免 401 触发爆破限速后干扰后续断言
	ipSeq := 0
	do := func(method, path string, withAdmin, withPanel bool, body io.Reader) int {
		ipSeq++
		req := httptest.NewRequest(method, path, body)
		req.RemoteAddr = "10.0.0." + strconv.Itoa(ipSeq) + ":1234"
		if withAdmin {
			req.Header.Set("X-Admin-Token", adminTok)
		}
		if withPanel {
			req.SetBasicAuth("panel", "panel-password-123")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}

	// ★ 长连接端点必须单独处理：httptest.NewRecorder 上跑 SSE handler
	// 会进入读循环直到客户端断开，而 Recorder 永远不会断开 ——
	// 结果是整个测试超时（不是断言失败，更难定位）。
	// 这里只验证"无凭据必须 401"，带凭据的成功路径由
	// TestStreamSendsHelloAndChange 用真实 server + 读首帧覆盖。
	streaming := map[string]bool{"/api/stream": true}

	for _, rt := range srv.routes(http.NotFoundHandler()) {
		path := rt.pattern
		if streaming[rt.pattern] {
			if code := do(http.MethodGet, path, false, false, nil); code != http.StatusUnauthorized {
				t.Errorf("%s 无凭据应 401，实际 %d", rt.pattern, code)
			}
			continue
		}
		if strings.HasSuffix(path, "/") && path != "/" {
			path += "probe"
		}
		switch rt.level {
		case authPublic:
			if code := do(http.MethodGet, path, false, false, nil); code == http.StatusUnauthorized {
				t.Errorf("%s 应公开，实际 401", rt.pattern)
			}
		case authNodeToken:
			if code := do(http.MethodPost, path, false, false, strings.NewReader("{}")); code != http.StatusUnauthorized {
				t.Errorf("%s 无效节点令牌应 401，实际 %d", rt.pattern, code)
			}
		case authLogin:
			// 登录/会话端点自身不套鉴权中间件，未登录探测也不能 401
			// （否则正常的首屏会话检查会被计入爆破限速）。
			if code := do(http.MethodGet, path, false, false, nil); code == http.StatusUnauthorized {
				t.Errorf("%s 属于登录端点，不应由中间件返回 401，实际 %d", rt.pattern, code)
			}
		case authReadonly:
			if code := do(http.MethodGet, path, false, false, nil); code != http.StatusUnauthorized {
				t.Errorf("%s 无凭据应 401，实际 %d", rt.pattern, code)
			}
			if code := do(http.MethodGet, path, true, false, nil); code == http.StatusUnauthorized {
				t.Errorf("%s 带管理令牌不应 401，实际 %d", rt.pattern, code)
			}
			if code := do(http.MethodGet, path, false, true, nil); code == http.StatusUnauthorized {
				t.Errorf("%s 带面板凭据不应 401，实际 %d", rt.pattern, code)
			}
		case authAdmin:
			if code := do(http.MethodGet, path, false, false, nil); code != http.StatusUnauthorized {
				t.Errorf("%s 无凭据应 401，实际 %d", rt.pattern, code)
			}
			if code := do(http.MethodGet, path, false, true, nil); code != http.StatusUnauthorized {
				t.Errorf("%s 面板凭据不应通过，实际 %d", rt.pattern, code)
			}
			if code := do(http.MethodGet, path, true, false, nil); code == http.StatusUnauthorized {
				t.Errorf("%s 带管理令牌不应 401，实际 %d", rt.pattern, code)
			}
		case authMetrics:
			if code := do(http.MethodGet, path, false, false, nil); code != http.StatusUnauthorized {
				t.Errorf("%s 无凭据应 401，实际 %d", rt.pattern, code)
			}
			if code := do(http.MethodGet, path, true, false, nil); code != http.StatusOK {
				t.Errorf("%s 带管理令牌应 200，实际 %d", rt.pattern, code)
			}
			if code := do(http.MethodGet, path, false, true, nil); code != http.StatusOK {
				t.Errorf("%s 带面板凭据应 200，实际 %d", rt.pattern, code)
			}
		default:
			t.Errorf("%s 出现未分类路由级别 %d", rt.pattern, rt.level)
		}
	}
}

// TestNodeStatesAPI 校验 no_data / online / stale / offline 在
// 节点列表、总览和公开状态页上的语义与计数一致。
func TestNodeStatesAPI(t *testing.T) {
	const adminTok = "test-admin-token-1234567890"
	now := time.Now().Unix()
	st := store.New(t.TempDir())
	for _, n := range []struct{ id, token string }{
		{"fresh", "tok-fresh"}, {"online", "tok-online"}, {"stale", "tok-stale"}, {"off", "tok-off"},
	} {
		st.UpsertNode(&model.Node{ID: n.id, Name: n.id, Token: n.token})
	}
	st.AddMetrics("online", model.HostInfo{}, model.Metrics{TS: now, CPUPct: 10})
	st.AddMetrics("stale", model.HostInfo{}, model.Metrics{TS: now - store.OnlineWindow - 10, CPUPct: 20})
	st.AddMetrics("off", model.HostInfo{}, model.Metrics{TS: now - store.StaleWindow - 10, CPUPct: 30})

	srv := New(st, nil, nil, Config{AdminToken: adminTok})
	h := srv.Handler()

	get := func(path string, out any, admin bool) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if admin {
			req.Header.Set("X-Admin-Token", adminTok)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s 应 200，实际 %d: %s", path, w.Code, w.Body.String())
		}
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatalf("%s 响应不是 JSON: %v", path, err)
		}
	}

	var views []model.NodeView
	get("/api/nodes", &views, true)
	got := make(map[string]model.NodeView, len(views))
	for _, v := range views {
		got[v.ID] = v
	}
	wantStates := map[string]string{
		"fresh": model.NodeStateNoData, "online": model.NodeStateOnline,
		"stale": model.NodeStateStale, "off": model.NodeStateOffline,
	}
	for id, want := range wantStates {
		v, ok := got[id]
		if !ok {
			t.Fatalf("节点 %s 未出现在 /api/nodes", id)
		}
		if v.State != want {
			t.Errorf("节点 %s state=%q，期望 %q", id, v.State, want)
		}
		if v.Online != (want == model.NodeStateOnline) {
			t.Errorf("节点 %s online=%v，与 state=%q 不一致", id, v.Online, want)
		}
	}
	if got["fresh"].Latest.TS != 0 {
		t.Errorf("待接入节点不应带指标，实际 TS=%d", got["fresh"].Latest.TS)
	}

	var ov model.Overview
	get("/api/overview", &ov, true)
	if ov.Nodes != 4 || ov.Online != 1 || ov.Stale != 1 || ov.Offline != 1 || ov.NoData != 1 {
		t.Errorf("总览计数不符: %+v", ov)
	}

	var pub struct {
		Online  int `json:"online"`
		Stale   int `json:"stale"`
		Offline int `json:"offline"`
		NoData  int `json:"noData"`
		Total   int `json:"total"`
		Nodes   []struct {
			Name   string `json:"name"`
			State  string `json:"state"`
			Online bool   `json:"online"`
		} `json:"nodes"`
	}
	get("/api/status", &pub, false)
	if pub.Total != 4 || pub.Online != 1 || pub.Stale != 1 || pub.Offline != 1 || pub.NoData != 1 {
		t.Errorf("公开状态页计数不符: %+v", pub)
	}

	var detail struct {
		State  string `json:"state"`
		Online bool   `json:"online"`
	}
	get("/api/nodes/fresh", &detail, true)
	if detail.State != model.NodeStateNoData || detail.Online {
		t.Errorf("待接入节点详情应为 no_data/offline，实际 %+v", detail)
	}
}

// TestAgentVersionWireAndOutdatedHint 验证版本经请求头上报、管理视图返回
// 可升级提示，且公开状态页不会带出版本信息。
func TestAgentVersionWireAndOutdatedHint(t *testing.T) {
	const adminTok = "test-admin-token-1234567890"
	oldVersion := version.Version
	version.Version = "v1.9.3"
	defer func() { version.Version = oldVersion }()

	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "n1", Token: "tok-n1"})
	srv := New(st, nil, nil, Config{AdminToken: adminTok})
	h := srv.Handler()

	report := `{"nodeId":"n1","host":{"hostname":"h"},"metrics":{}}`
	req := httptest.NewRequest(http.MethodPost, "/api/report", strings.NewReader(report))
	req.Header.Set("X-Node-Token", "tok-n1")
	req.Header.Set("X-Agent-Version", "v1.9.2")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("上报失败: %d %s", w.Code, w.Body.String())
	}
	n, _ := st.Node("n1")
	if n.AgentVersion != "v1.9.2" {
		t.Fatalf("未记录 Agent 版本: %+v", n)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/admin/nodes", nil)
	req.Header.Set("X-Admin-Token", adminTok)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var admin []adminNodeView
	if err := json.Unmarshal(w.Body.Bytes(), &admin); err != nil {
		t.Fatalf("管理节点响应不是 JSON: %v", err)
	}
	if len(admin) != 1 || admin[0].AgentVersion != "v1.9.2" || !admin[0].AgentOutdated {
		t.Fatalf("管理节点视图缺少版本/过期提示: %+v", admin)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/nodes/n1", nil)
	req.Header.Set("X-Admin-Token", adminTok)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var detail struct {
		AgentOutdated bool   `json:"agentOutdated"`
		ServerVersion string `json:"serverVersion"`
		Node          struct {
			AgentVersion string `json:"agentVersion"`
		} `json:"node"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatalf("节点详情不是 JSON: %v", err)
	}
	if detail.Node.AgentVersion != "v1.9.2" || !detail.AgentOutdated || detail.ServerVersion != "v1.9.3" {
		t.Fatalf("节点详情版本信息不符: %+v", detail)
	}

	// 公开状态页不能因为内部视图新增字段而泄露 Agent 版本
	req = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if strings.Contains(w.Body.String(), "agentVersion") {
		t.Fatal("公开状态页不应包含 agentVersion")
	}
}

func TestVersionOutdated(t *testing.T) {
	cases := []struct {
		agent, server string
		want          bool
	}{
		{"v1.9.2", "v1.9.3", true},
		{"1.9.2", "v1.9.3", true},
		{"v1.9.3", "v1.9.3", false},
		{"v2.0.0", "v1.9.3", false},
		{"dev", "v1.9.3", false},
		{"v1.9.2", "dev", false},
	}
	for _, c := range cases {
		if got := versionOutdated(c.agent, c.server); got != c.want {
			t.Errorf("versionOutdated(%q,%q)=%v want %v", c.agent, c.server, got, c.want)
		}
	}
}

// TestNodePUTUpdatesMetadataPreservesToken 校验台账编辑只改元数据，
// ID 与上报令牌保持不变（否则一次改名会让 Agent 立即 401）。
func TestNodePUTUpdatesMetadataPreservesToken(t *testing.T) {
	const adminTok = "test-admin-token-1234567890"
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{
		ID: "n1", Name: "旧名", Provider: "old", Region: "old", Group: "old",
		Note: "old", ExpireAt: "2027-01-02", Price: 100, Currency: "USD",
		Weight: 5, Token: "original-token",
	})
	srv := New(st, nil, nil, Config{AdminToken: adminTok})

	body := []byte(`{"name":"  新展示名  ","provider":"newp","region":"sg","group":"prod",
		"note":"备注","expireAt":"2027-03-04","price":120.5,"currency":"cny","weight":1}`)
	req := httptest.NewRequest(http.MethodPut, "/api/admin/nodes/n1", bytes.NewReader(body))
	req.Header.Set("X-Admin-Token", adminTok)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT 返回 %d: %s", w.Code, w.Body.String())
	}

	got, ok := st.Node("n1")
	if !ok {
		t.Fatal("节点丢失")
	}
	if got.ID != "n1" || got.Token != "original-token" {
		t.Errorf("ID/令牌被改动: id=%q token=%q", got.ID, got.Token)
	}
	if got.Name != "新展示名" || got.Provider != "newp" || got.Region != "sg" ||
		got.Group != "prod" || got.Note != "备注" || got.ExpireAt != "2027-03-04" ||
		got.Price != 120.5 || got.Currency != "CNY" || got.Weight != 1 {
		t.Errorf("元数据未正确更新: %+v", got)
	}

	// 不存在与非法输入
	req = httptest.NewRequest(http.MethodPut, "/api/admin/nodes/nope", bytes.NewReader([]byte(`{"name":"x"}`)))
	req.Header.Set("X-Admin-Token", adminTok)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("不存在的节点应 404，实际 %d", w.Code)
	}

	bad := []string{
		`{"name":""}`,
		`{"name":"x","currency":"EUR"}`,
		`{"name":"x","price":-1}`,
		`{"name":"x","expireAt":"2027-13-01"}`,
	}
	for _, b := range bad {
		req = httptest.NewRequest(http.MethodPut, "/api/admin/nodes/n1", bytes.NewReader([]byte(b)))
		req.Header.Set("X-Admin-Token", adminTok)
		w = httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("非法输入 %s 应 400，实际 %d", b, w.Code)
		}
	}
}

// TestRotateNodeToken 验证令牌轮换：旧令牌立即失效、新令牌可上报，
// 且轮换结果已落盘（重启后仍有效）。
func TestRotateNodeToken(t *testing.T) {
	const adminTok = "test-admin-token-1234567890"
	dir := t.TempDir()
	st := store.New(dir)
	st.UpsertNode(&model.Node{ID: "n1", Name: "节点一", Token: "old-token"})
	srv := New(st, nil, nil, Config{AdminToken: adminTok})
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/admin/nodes/n1/rotate-token", nil)
	req.Header.Set("X-Admin-Token", adminTok)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET 轮换应 405，实际 %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/admin/nodes/n1/rotate-token", nil)
	req.Header.Set("X-Admin-Token", adminTok)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("轮换返回 %d: %s", w.Code, w.Body.String())
	}
	var view adminNodeView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if view.Token == "" || view.Token == "old-token" || len(view.Token) < 32 {
		t.Fatalf("未生成有效新令牌: %q", view.Token)
	}

	report := []byte(`{"nodeId":"n1","host":{"hostname":"h"},"metrics":{"cpuPct":1}}`)
	send := func(token string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/report", bytes.NewReader(report))
		req.Header.Set("X-Node-Token", token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	if code := send("old-token"); code != http.StatusUnauthorized {
		t.Errorf("旧令牌应失效，实际 %d", code)
	}
	if code := send(view.Token); code != http.StatusOK {
		t.Errorf("新令牌应可上报，实际 %d", code)
	}

	re := store.New(dir)
	if err := re.LoadErr(); err != nil {
		t.Fatalf("重载快照: %v", err)
	}
	if got, _ := re.Node("n1"); got.Token != view.Token {
		t.Errorf("新令牌未持久化: got %q", got.Token)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/admin/nodes/nope/rotate-token", nil)
	req.Header.Set("X-Admin-Token", adminTok)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("不存在的节点应 404，实际 %d", w.Code)
	}
}

func TestNormalizeScope(t *testing.T) {
	got := normalizeScope([]string{" prod ", "test", "prod", "", "  "})
	if len(got) != 2 || got[0] != "prod" || got[1] != "test" {
		t.Fatalf("去空白/去重失败: %#v", got)
	}

	many := make([]string, 250)
	for i := range many {
		many[i] = "g" + strconv.Itoa(i)
	}
	if got := normalizeScope(many); len(got) != 200 {
		t.Errorf("作用域应截断到 200，实际 %d", len(got))
	}
	if got := normalizeScope(nil); len(got) != 0 {
		t.Errorf("空输入应返回空切片，实际 %#v", got)
	}
}

func TestMonthlyUsesConfiguredRate(t *testing.T) {
	if got := monthly(86.4, "CNY", 7.2); got < 0.999 || got > 1.001 {
		t.Errorf("CNY 按月折算错误: %.4f", got)
	}
	if got := monthly(86.4, "CNY", 0); got < 0.999 || got > 1.001 {
		t.Errorf("汇率缺失应回退 7.2: %.4f", got)
	}
	if got := monthly(12, "USD", 7.2); got < 0.999 || got > 1.001 {
		t.Errorf("USD 不应受汇率影响: %.4f", got)
	}
}

// TestPublicStatusIsDesensitized 确认公开状态页不泄露资产与指纹信息。
func TestPublicStatusIsDesensitized(t *testing.T) {
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{
		ID: "n1", Name: "公开展示名", Provider: "secret-provider", Region: "secret-region",
		Group: "prod", Note: "secret-note", ExpireAt: "2027-01-01", Price: 999,
		Currency: "USD", Token: "secret-node-token",
	})
	st.AddMetrics("n1", model.HostInfo{
		Hostname: "secret-hostname", OS: "secret-os", Kernel: "secret-kernel",
		Arch: "secret-arch", CPUModel: "secret-cpu",
	}, model.Metrics{TS: time.Now().Unix(), CPUPct: 10})

	srv := New(st, nil, nil, Config{AdminToken: "test-admin-token-1234567890"})
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/api/status 返回 %d", w.Code)
	}
	body := w.Body.String()
	for _, secret := range []string{
		"secret-provider", "secret-region", "secret-note", "secret-hostname",
		"secret-os", "secret-kernel", "secret-arch", "secret-cpu",
		"secret-node-token", `"id"`, `"expireAt"`, `"price"`,
	} {
		if bytes.Contains([]byte(body), []byte(secret)) {
			t.Errorf("公开状态页泄露 %q: %s", secret, body)
		}
	}
	if !bytes.Contains([]byte(body), []byte("公开展示名")) {
		t.Errorf("公开状态页应包含展示名: %s", body)
	}
}

// TestReportStillUsesNodeToken 确认只读接口加固不会影响 Agent 上报。
func TestReportStillUsesNodeToken(t *testing.T) {
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "节点", Token: "node-token"})
	srv := New(st, nil, nil, Config{AdminToken: "test-admin-token-1234567890"})
	h := srv.Handler()

	report := []byte(`{"nodeId":"n1","host":{"hostname":"h"},"metrics":{"cpuPct":1}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/report", bytes.NewReader(report))
	req.Header.Set("X-Node-Token", "node-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("合法上报应 200，实际 %d: %s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/report", bytes.NewReader(report))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("无节点令牌应 401，实际 %d", w.Code)
	}
}

// TestReportHostIsSanitized 是回归测试（借鉴 Heron 的 sanitize）。
//
// Agent 自报的文本会原样进入内存快照、公开状态页、CSV 导出、Prometheus
// 输出与服务端日志，而 Agent 是可能被攻破的一端。曾经的缺陷是这些字符串
// 完全不经过清洗：一条带换行的主机名能把一条日志伪装成两条，带双向控制符
// 的系统名能在终端里显示成与真实字节序不同的样子。
func TestReportHostIsSanitized(t *testing.T) {
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "节点", Token: "node-token"})
	srv := New(st, nil, nil, Config{AdminToken: "test-admin-token-1234567890"})
	h := srv.Handler()

	// \n 伪造日志换行；U+202E（RLO）反转后续文本的显示方向；cpuModel 超长
	report := []byte(`{"nodeId":"n1","host":{"hostname":"web-01\nALERT: ok",` +
		`"os":"ubuntu\u202egood","cpuModel":"ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789` +
		`ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789` +
		`ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"},"metrics":{"cpuPct":1}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/report", bytes.NewReader(report))
	req.Header.Set("X-Node-Token", "node-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("合法上报应 200，实际 %d: %s", w.Code, w.Body.String())
	}

	got := st.Host("n1")
	if strings.ContainsAny(got.Hostname, "\r\n") {
		t.Errorf("主机名仍含换行，可伪造日志: %q", got.Hostname)
	}
	if got.Hostname != "web-01ALERT: ok" {
		t.Errorf("主机名 = %q，期望 %q", got.Hostname, "web-01ALERT: ok")
	}
	if strings.ContainsRune(got.OS, '\u202e') {
		t.Errorf("系统名仍含双向控制符 U+202E: %q", got.OS)
	}
	// 超长字段必须被截断，且不能截出半个字符
	if len(got.CPUModel) > 128 {
		t.Errorf("cpuModel 未被限长: %d 字节", len(got.CPUModel))
	}
	if !utf8.ValidString(got.CPUModel) {
		t.Errorf("cpuModel 截断后不是合法 UTF-8: %q", got.CPUModel)
	}
}

// TestSilenceAPI 验证静默的鉴权、校验与生效范围。
func TestSilenceAPI(t *testing.T) {
	st := store.New(t.TempDir())
	st.UpsertRule(&model.AlertRule{ID: "r_cpu", Name: "CPU", Enabled: true})
	srv := New(st, nil, nil, Config{AdminToken: "test-admin-token-1234567890"})
	h := srv.Handler()

	do := func(method, path, body, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		if token != "" {
			req.Header.Set("X-Admin-Token", token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	if w := do(http.MethodPut, "/api/admin/silences/global", `{"minutes":60}`, ""); w.Code != 401 {
		t.Fatalf("无令牌应 401，实际 %d", w.Code)
	}
	if w := do(http.MethodPut, "/api/admin/silences/global", `{"minutes":0}`, "test-admin-token-1234567890"); w.Code != 400 {
		t.Fatalf("非法时长应 400，实际 %d", w.Code)
	}
	if w := do(http.MethodPut, "/api/admin/silences/r_missing", `{"minutes":60}`, "test-admin-token-1234567890"); w.Code != 404 {
		t.Fatalf("不存在的规则应 404，实际 %d", w.Code)
	}
	// 规则级静默 + 全局静默
	if w := do(http.MethodPut, "/api/admin/silences/r_cpu", `{"minutes":30,"reason":"升级"}`, "test-admin-token-1234567890"); w.Code != 200 {
		t.Fatalf("规则静默失败: %d %s", w.Code, w.Body.String())
	}
	if w := do(http.MethodPut, "/api/admin/silences/global", `{"minutes":60,"reason":"维护"}`, "test-admin-token-1234567890"); w.Code != 200 {
		t.Fatalf("全局静默失败: %d %s", w.Code, w.Body.String())
	}

	w := do(http.MethodGet, "/api/admin/silences", "", "test-admin-token-1234567890")
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"global"`)) ||
		!bytes.Contains(w.Body.Bytes(), []byte(`"r_cpu"`)) {
		t.Fatalf("静默列表异常: %d %s", w.Code, w.Body.String())
	}

	// 只读告警接口应携带静默状态与队列统计
	w = do(http.MethodGet, "/api/alerts", "", "test-admin-token-1234567890")
	for _, want := range []string{`"silences"`, `"notify"`, `"pending"`, `"suppressed"`} {
		if !bytes.Contains(w.Body.Bytes(), []byte(want)) {
			t.Fatalf("/api/alerts 缺少 %s: %s", want, w.Body.String())
		}
	}

	if w := do(http.MethodDelete, "/api/admin/silences/global", "", "test-admin-token-1234567890"); w.Code != 200 ||
		!bytes.Contains(w.Body.Bytes(), []byte(`"deleted":true`)) {
		t.Fatalf("取消静默失败: %d %s", w.Code, w.Body.String())
	}
	if got := st.Silences(time.Now().Unix()); len(got) != 1 || got[0].Scope != "r_cpu" {
		t.Fatalf("取消 global 后仅应保留规则静默: %+v", got)
	}
}

// TestMetricsExposeNotifyQueue 验证通知队列、落盘、历史分层与状态指标。
func TestMetricsExposeNotifyQueue(t *testing.T) {
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "节点一", Token: "tok"})
	now := time.Now().Unix()
	st.SetSilence(&model.Silence{Scope: "global", Until: now + 600})
	st.EnqueueNotifyJob(&model.NotifyJob{
		ID: "nj_m", Severity: "critical", Status: model.NotifyPending,
		CreatedAt: now, NextAttemptAt: now,
	})
	if err := st.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	srv := New(st, nil, nil, Config{AdminToken: "test-admin-token-1234567890"})
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("X-Admin-Token", "test-admin-token-1234567890")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("/metrics 返回 %d", w.Code)
	}
	for _, want := range []string{
		"rollcall_notify_queue_pending 1",
		"rollcall_notify_sent_total 0",
		"rollcall_notify_failed_total 0",
		"rollcall_notify_retries_total 0",
		"rollcall_notify_suppressed_total 0",
		"rollcall_notify_rejected_total 0",
		"rollcall_notify_last_success_timestamp_seconds 0",
		"rollcall_notify_silences_active 1",
		"rollcall_save_total 1",
		"rollcall_save_failures_total 0",
		"rollcall_nodes_no_data 1",
		`rollcall_node_state{id="n1",name="节点一",state="no_data"} 1`,
		`rollcall_history_points{id="n1",name="节点一",resolution="1m"} 0`,
		`rollcall_history_points{id="n1",name="节点一",resolution="5m"} 0`,
		`rollcall_history_points{id="n1",name="节点一",resolution="1h"} 0`,
	} {
		if !bytes.Contains(w.Body.Bytes(), []byte(want)) {
			t.Fatalf("指标缺失 %q:\n%s", want, w.Body.String())
		}
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("rollcall_save_duration_seconds ")) {
		t.Fatalf("缺少落盘耗时指标:\n%s", w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("rollcall_save_last_success_timestamp_seconds ")) {
		t.Fatalf("缺少落盘时间戳指标:\n%s", w.Body.String())
	}
}

// TestNodeSeriesRangeResolution 验证长范围自动切换到聚合分辨率，
// 并且聚合点带有增量字段供前端与 CSV 使用。
func TestNodeSeriesRangeResolution(t *testing.T) {
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "节点一"})
	now := time.Now().Unix()
	for ts := now - 7200; ts <= now; ts += 30 {
		st.AddMetrics("n1", model.HostInfo{}, model.Metrics{
			TS: ts, CPUPct: 10, Load1: 1,
			MemUsed: 1 << 30, MemTotal: 4 << 30,
			DiskUsed: 1 << 30, DiskTotal: 10 << 30,
			NetRx: uint64(ts), NetTx: uint64(ts / 2),
		})
	}
	srv := New(st, nil, nil, Config{AdminToken: "test-admin-token-1234567890"})
	h := srv.Handler()
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Admin-Token", "test-admin-token-1234567890")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	cases := []struct {
		rng, resolution string
		minPts          int
	}{
		{"1h", "raw", 100},
		{"6h", "raw", 200},
		{"24h", "raw", 200},
		{"7d", "1m", 100},
		{"30d", "5m", 20},
		{"365d", "1h", 1},
	}
	for _, c := range cases {
		w := get("/api/nodes/n1/series?range=" + c.rng)
		if w.Code != 200 {
			t.Fatalf("%s 返回 %d: %s", c.rng, w.Code, w.Body.String())
		}
		var got struct {
			Resolution string           `json:"resolution"`
			Points     []map[string]any `json:"points"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s 响应解析失败: %v", c.rng, err)
		}
		if got.Resolution != c.resolution {
			t.Errorf("%s 分辨率 %q，want %q", c.rng, got.Resolution, c.resolution)
		}
		if len(got.Points) < c.minPts {
			t.Errorf("%s 点数 %d，want ≥%d", c.rng, len(got.Points), c.minPts)
		}
		if c.resolution != "raw" && len(got.Points) > 0 {
			if _, ok := got.Points[0]["count"]; !ok {
				t.Errorf("%s 聚合点缺少 count", c.rng)
			}
			if _, ok := got.Points[0]["netRxDelta"]; !ok {
				t.Errorf("%s 聚合点缺少 netRxDelta", c.rng)
			}
		}
	}

	if w := get("/api/nodes/n1/series?range=2y"); w.Code != 400 {
		t.Errorf("未知范围应 400，实际 %d", w.Code)
	}

	w := get("/api/export/metrics.csv?node=n1&range=7d")
	if w.Code != 200 {
		t.Fatalf("7d 导出返回 %d: %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("分辨率")) || !bytes.Contains(w.Body.Bytes(), []byte(",1m,")) {
		t.Errorf("7d 导出缺少聚合列: %s", w.Body.String())
	}
}

// TestGzipAPIResponses 验证长范围 JSON 与 CSV 在客户端声明 gzip 时被压缩，
// 未声明时保持原样，避免慢速链路上大响应长时间挂起。
func TestGzipAPIResponses(t *testing.T) {
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "节点一"})
	now := time.Now().Unix()
	for ts := now - 7200; ts <= now; ts += 30 {
		st.AddMetrics("n1", model.HostInfo{}, model.Metrics{
			TS: ts, CPUPct: 10, Load1: 1,
			MemUsed: 1 << 30, MemTotal: 4 << 30,
			DiskUsed: 1 << 30, DiskTotal: 10 << 30,
			NetRx: uint64(ts), NetTx: uint64(ts / 2),
		})
	}
	srv := New(st, nil, nil, Config{AdminToken: "test-admin-token-1234567890"})
	h := srv.Handler()

	do := func(path string, acceptGzip bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Admin-Token", "test-admin-token-1234567890")
		if acceptGzip {
			req.Header.Set("Accept-Encoding", "gzip, deflate")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	readBody := func(t *testing.T, w *httptest.ResponseRecorder) []byte {
		t.Helper()
		if enc := w.Header().Get("Content-Encoding"); enc != "gzip" {
			t.Fatalf("Content-Encoding=%q，want gzip（header=%v）", enc, w.Header())
		}
		zr, err := gzip.NewReader(w.Body)
		if err != nil {
			t.Fatalf("响应不是合法 gzip: %v", err)
		}
		defer zr.Close()
		body, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("解压失败: %v", err)
		}
		return body
	}

	for _, path := range []string{"/api/nodes/n1/series?range=7d", "/api/overview"} {
		gz := do(path, true)
		if gz.Code != 200 {
			t.Fatalf("%s 返回 %d", path, gz.Code)
		}
		if !strings.Contains(gz.Header().Get("Vary"), "Accept-Encoding") {
			t.Errorf("%s 缺少 Vary: Accept-Encoding", path)
		}
		plain := do(path, false)
		if enc := plain.Header().Get("Content-Encoding"); enc != "" {
			t.Errorf("%s 未声明 gzip 却返回 Content-Encoding=%q", path, enc)
		}
		if !bytes.Equal(readBody(t, gz), plain.Body.Bytes()) {
			t.Errorf("%s 解压内容与未压缩响应不一致", path)
		}
		if gz.Body.Len() >= plain.Body.Len() {
			t.Errorf("%s 压缩后 %d 字节，未小于原始 %d 字节", path, gz.Body.Len(), plain.Body.Len())
		}
	}

	csv := do("/api/export/metrics.csv?node=n1&range=7d", true)
	if csv.Code != 200 {
		t.Fatalf("CSV 导出返回 %d", csv.Code)
	}
	if !bytes.Contains(readBody(t, csv), []byte("分辨率")) {
		t.Error("压缩后的 CSV 内容不正确")
	}
}

func TestAdminNotifyJobsListAndRetry(t *testing.T) {
	const token = "test-admin-token-1234567890"
	st := store.New(t.TempDir())
	srv := New(st, nil, nil, Config{AdminToken: token})
	now := time.Now().Unix()
	if !st.EnqueueNotifyJob(&model.NotifyJob{
		ID: "nj1", RuleID: "r1", Title: "节点失联", Status: model.NotifyFailed,
		Attempts: 5, LastError: "boom", CreatedAt: now,
	}) {
		t.Fatal("入队失败")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/notify/jobs", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("无令牌读取队列应 401，实际 %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/admin/notify/jobs", nil)
	req.Header.Set("X-Admin-Token", token)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("读取队列返回 %d", w.Code)
	}
	var out struct {
		Stats model.NotifyStats  `json:"stats"`
		Jobs  []*model.NotifyJob `json:"jobs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析队列响应: %v", err)
	}
	if len(out.Jobs) != 1 || out.Jobs[0].ID != "nj1" || out.Jobs[0].Status != model.NotifyFailed {
		t.Fatalf("队列内容异常: %+v", out.Jobs)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/admin/notify/jobs/nj1/retry", nil)
	req.Header.Set("X-Admin-Token", token)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("手动重试返回 %d: %s", w.Code, w.Body.String())
	}
	jobs := st.NotifyJobs(10)
	if len(jobs) != 1 || jobs[0].Status != model.NotifyPending || jobs[0].Attempts != 0 || jobs[0].LastError != "" {
		t.Fatalf("重试后任务状态异常: %+v", jobs)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/admin/notify/jobs/nj1/retry", nil)
	req.Header.Set("X-Admin-Token", token)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("重复重试应 409，实际 %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/admin/notify/jobs/missing/retry", nil)
	req.Header.Set("X-Admin-Token", token)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("重试不存在的任务应 404，实际 %d", w.Code)
	}
}

func TestDeleteRuleSendsRecoveryNotification(t *testing.T) {
	const token = "test-admin-token-1234567890"
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "节点一"})
	st.UpsertRule(&model.AlertRule{
		ID: "r1", Name: "节点失联", Metric: "offline", Op: "gt",
		Severity: "critical", Enabled: true,
	})
	now := time.Now().Unix()
	if created, ok := st.OpenAlert(&model.Alert{
		ID: "a1", RuleID: "r1", RuleName: "节点失联",
		NodeID: "n1", NodeName: "节点一", Severity: "critical", StartedAt: now,
	}); !created || !ok {
		t.Fatal("准备活跃告警失败")
	}

	e := alert.New(st)
	srv := New(st, nil, e, Config{AdminToken: token})
	var sent []notify.Message
	e.SetNotifier(func(m notify.Message) { sent = append(sent, m) })

	req := httptest.NewRequest(http.MethodDelete, "/api/admin/rules/r1", nil)
	req.Header.Set("X-Admin-Token", token)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("删除规则返回 %d: %s", w.Code, w.Body.String())
	}
	if st.ActiveAlertCount() != 0 {
		t.Fatalf("删除规则后仍有 %d 条活跃告警", st.ActiveAlertCount())
	}
	if len(sent) != 1 || !sent[0].Resolved || sent[0].AlertID != "a1" {
		t.Fatalf("删除规则应补发 1 条恢复通知，实际 %+v", sent)
	}
	if !strings.Contains(sent[0].Body, "规则已删除") {
		t.Errorf("恢复通知未说明删除原因: %q", sent[0].Body)
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/admin/rules/r_missing", nil)
	req.Header.Set("X-Admin-Token", token)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("删除不存在的规则应 404，实际 %d", w.Code)
	}
}

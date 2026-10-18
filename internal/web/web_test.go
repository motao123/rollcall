package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/motao/rollcall/internal/model"
	"github.com/motao/rollcall/internal/store"
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
		got := sanitize(c.in)
		if got != c.want {
			t.Errorf("sanitize(%q) = %q, 期望 %q", c.in, got, c.want)
		}
		// 无论输入如何，输出绝不能含 CR/LF
		if bytes.ContainsAny([]byte(got), "\r\n") {
			t.Errorf("sanitize(%q) 仍含 CR/LF: %q", c.in, got)
		}
	}
}

// TestNodeUpdatePreservesToken 是回归测试。
//
// 曾经的缺陷：POST /api/admin/nodes 无条件 in.Token = store.NewToken()，
// 于是带着已存在 ID 更新台账会**静默轮换令牌**，该节点 Agent 立即开始 401，
// 与之前 json:"-" 令牌持久化 bug 同类（都是重启/更新后令牌失效）。
func TestNodeUpdatePreservesToken(t *testing.T) {
	st := store.New(t.TempDir())
	const adminTok = "test-admin-token-1234567890"

	// 预置一个已有令牌的节点
	n := &model.Node{ID: "n1", Name: "节点一", Provider: "old", Token: "original-token"}
	st.UpsertNode(n)

	srv := New(st, nil, nil, Config{AdminToken: adminTok})

	// 用同一 ID 更新台账（改名/改服务商），不应触碰令牌
	body, _ := json.Marshal(model.Node{ID: "n1", Name: "节点一改", Provider: "new"})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/nodes", bytes.NewReader(body))
	req.Header.Set("X-Admin-Token", adminTok)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != 201 {
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

// TestPublicNodeViewDoesNotCarryToken 防止视图层携带上报令牌：
// json:"-" 只挡住标准 JSON，内存中的副本也不应保留秘密。
func TestPublicNodeViewDoesNotCarryToken(t *testing.T) {
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "节点一", Token: "secret-token"})
	srv := New(st, nil, nil, Config{})
	views := srv.buildNodeViews()
	if len(views) != 1 {
		t.Fatalf("视图数 = %d，期望 1", len(views))
	}
	if views[0].Token != "" {
		t.Errorf("公开节点视图不应携带令牌，实际 %q", views[0].Token)
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

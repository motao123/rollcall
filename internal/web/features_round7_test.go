package web

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/auth"
	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/notify"
	"github.com/motao123/rollcall/internal/probe"
	"github.com/motao123/rollcall/internal/store"
)

const testAdminToken = "test-admin-token-1234567890"

func adminReq(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("X-Admin-Token", testAdminToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func bearerReq(h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAPITokenScopes(t *testing.T) {
	st := store.New(t.TempDir())
	h := New(st, nil, nil, Config{AdminToken: testAdminToken}).Handler()

	w := adminReq(h, http.MethodPost, "/api/admin/tokens", `{"name":"监控只读","scopes":["read"],"expiresDays":30}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("创建令牌应 201，实际 %d: %s", w.Code, w.Body.String())
	}
	var created struct {
		Token string         `json:"token"`
		Meta  model.APIToken `json:"meta"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("解析令牌响应失败: %v", err)
	}
	if !strings.HasPrefix(created.Token, APITokenPrefix) {
		t.Errorf("令牌前缀错误: %q", created.Token)
	}
	if created.Meta.ID == "" || created.Meta.Prefix != created.Token[:12] || created.Meta.ExpiresAt <= time.Now().Unix() {
		t.Errorf("令牌元数据错误: %+v", created.Meta)
	}
	if strings.Contains(w.Body.String(), `"hash"`) {
		t.Error("响应不应包含令牌摘要")
	}

	// 只读令牌可访问只读接口。
	if w := bearerReq(h, http.MethodGet, "/api/overview", created.Token, ""); w.Code != http.StatusOK {
		t.Fatalf("read 令牌读取概览应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	// 但不能调用管理写接口。
	if w := bearerReq(h, http.MethodPost, "/api/admin/nodes", created.Token, `{"name":"token-node"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("read 令牌不应有管理权限，实际 %d", w.Code)
	}

	// admin 令牌可写。
	w = adminReq(h, http.MethodPost, "/api/admin/tokens", `{"name":"自动化","scopes":["admin"],"expiresDays":0}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("创建 admin 令牌失败: %s", w.Body.String())
	}
	var adminCreated struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &adminCreated); err != nil {
		t.Fatal(err)
	}
	if w := bearerReq(h, http.MethodPost, "/api/admin/nodes", adminCreated.Token, `{"name":"token-node"}`); w.Code != http.StatusCreated {
		t.Fatalf("admin 令牌应可创建节点，实际 %d: %s", w.Code, w.Body.String())
	}

	// 删除后立即失效。
	w = adminReq(h, http.MethodDelete, "/api/admin/tokens/"+created.Meta.ID, "")
	if w.Code != http.StatusOK {
		t.Fatalf("删除令牌失败: %s", w.Body.String())
	}
	if w := bearerReq(h, http.MethodGet, "/api/overview", created.Token, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("删除后令牌应立即失效，实际 %d", w.Code)
	}

	// 非法参数整单拒绝。
	for _, body := range []string{
		`{"name":"","scopes":["read"]}`,
		`{"name":"x","scopes":[]}`,
		`{"name":"x","scopes":["root"]}`,
		`{"name":"x","scopes":["read"],"expiresDays":3651}`,
		`{"name":"x","scopes":["read"],"oops":1}`,
	} {
		if w := adminReq(h, http.MethodPost, "/api/admin/tokens", body); w.Code != http.StatusBadRequest {
			t.Errorf("非法请求 %s 应 400，实际 %d", body, w.Code)
		}
	}
}

func TestRenewalsAPI(t *testing.T) {
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "节点一", Token: "t"})
	h := New(st, nil, nil, Config{AdminToken: testAdminToken}).Handler()

	w := adminReq(h, http.MethodPost, "/api/admin/renewals",
		`{"nodeId":"n1","at":"2026-10-01","amount":12.5,"currency":"usd","note":"月付"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("登记续费应 201，实际 %d: %s", w.Code, w.Body.String())
	}
	var created model.Renewal
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Currency != "USD" || created.CreatedAt == 0 {
		t.Errorf("续费记录未规范化: %+v", created)
	}

	w = adminReq(h, http.MethodGet, "/api/renewals", "")
	if w.Code != http.StatusOK {
		t.Fatalf("读取续费记录失败: %d", w.Code)
	}
	var rows []renewalView
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].NodeName != "节点一" || rows[0].Amount != 12.5 {
		t.Fatalf("续费列表内容错误: %+v", rows)
	}

	for _, body := range []string{
		`{"nodeId":"missing","at":"2026-10-01"}`,
		`{"nodeId":"n1","at":"10/01/2026"}`,
		`{"nodeId":"n1","at":"2026-10-01","amount":-1}`,
		`{"nodeId":"n1","at":"2026-10-01","currency":"EUR"}`,
	} {
		if w := adminReq(h, http.MethodPost, "/api/admin/renewals", body); w.Code != http.StatusBadRequest {
			t.Errorf("非法续费 %s 应 400，实际 %d", body, w.Code)
		}
	}

	if w := adminReq(h, http.MethodDelete, "/api/admin/renewals/"+created.ID, ""); w.Code != http.StatusOK {
		t.Fatalf("删除续费失败: %d", w.Code)
	}
	w = adminReq(h, http.MethodGet, "/api/renewals", "")
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("删除后应为空: %+v", rows)
	}
}

func TestServiceUpdateAPI(t *testing.T) {
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "节点一", Token: "t"})
	st.UpsertService(&model.Service{
		ID: "s1", NodeID: "n1", Name: "旧站点", Type: "http",
		Target: "https://example.com/", Interval: 60, Enabled: true,
	})
	h := New(st, nil, nil, Config{AdminToken: testAdminToken}).Handler()

	w := adminReq(h, http.MethodPut, "/api/admin/services/s1",
		`{"nodeId":"n1","name":"新站点","type":"https","target":"https://example.com/health","interval":30,"enabled":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("编辑服务应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var got model.Service
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "s1" || got.Name != "新站点" || got.Type != "https" || got.Interval != 30 || got.Enabled {
		t.Errorf("响应未反映更新: %+v", got)
	}
	if sv, ok := st.Service("s1"); !ok || sv.Name != "新站点" || sv.Target != "https://example.com/health" || sv.Enabled {
		t.Fatalf("更新未落库: %+v", sv)
	}

	// 未提供 enabled 时保持停用状态，不能被一次编辑悄悄启用。
	w = adminReq(h, http.MethodPut, "/api/admin/services/s1",
		`{"nodeId":"n1","name":"再改一次","type":"http","target":"https://example.com/"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("二次编辑应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	sv, _ := st.Service("s1")
	if sv.Enabled || sv.Interval != 60 || sv.Name != "再改一次" {
		t.Errorf("省略字段处理错误: %+v", sv)
	}

	if w := adminReq(h, http.MethodPut, "/api/admin/services/missing",
		`{"name":"x","type":"http","target":"https://example.com"}`); w.Code != http.StatusNotFound {
		t.Errorf("更新不存在的服务应 404，实际 %d", w.Code)
	}

	for _, body := range []string{
		`{"name":"","type":"http","target":"https://example.com"}`,
		`{"name":"x","type":"udp","target":"example.com"}`,
		`{"name":"x","type":"http","target":"  "}`,
		`{"name":"x","type":"http","target":"https://example.com","interval":9}`,
		`{"name":"x","type":"http","target":"https://example.com","interval":86401}`,
		`{"name":"x","type":"http","target":"https://example.com","oops":1}`,
	} {
		if w := adminReq(h, http.MethodPut, "/api/admin/services/s1", body); w.Code != http.StatusBadRequest {
			t.Errorf("非法请求 %s 应 400，实际 %d", body, w.Code)
		}
	}
	if sv, _ := st.Service("s1"); sv.Name != "再改一次" || sv.Interval != 60 {
		t.Errorf("被拒绝的请求不应改动服务: %+v", sv)
	}
}

func TestServiceProbeTestAPI(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	st := store.New(t.TempDir())
	st.UpsertService(&model.Service{
		ID: "s1", Name: "本地站点", Type: "http",
		Target: target.URL, Interval: 60, Enabled: true,
	})
	runner := probe.NewRunner(probe.Config{AllowPrivate: true})
	h := New(st, runner, nil, Config{AdminToken: testAdminToken}).Handler()

	w := adminReq(h, http.MethodPost, "/api/admin/services/s1/test", "")
	if w.Code != http.StatusOK {
		t.Fatalf("立即检测应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var res model.ServiceResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.ServiceID != "s1" || res.Status != http.StatusNoContent {
		t.Errorf("检测结果错误: %+v", res)
	}
	if sv, _ := st.Service("s1"); !sv.Enabled || sv.Interval != 60 {
		t.Errorf("立即检测不应改动服务定义: %+v", sv)
	}

	if w := adminReq(h, http.MethodGet, "/api/admin/services/s1/test", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET 检测端点应 405，实际 %d", w.Code)
	}
	if w := adminReq(h, http.MethodPost, "/api/admin/services/missing/test", ""); w.Code != http.StatusNotFound {
		t.Errorf("不存在的服务应 404，实际 %d", w.Code)
	}

	// 探测执行器未启用时明确返回 503，不 panic。
	h2 := New(st, nil, nil, Config{AdminToken: testAdminToken}).Handler()
	if w := adminReq(h2, http.MethodPost, "/api/admin/services/s1/test", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("缺少执行器应 503，实际 %d", w.Code)
	}
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "节点一", Token: "report-token-abc", Country: "JP", TrafficQuotaGB: 100})
	st.UpsertService(&model.Service{ID: "s1", NodeID: "n1", Name: "Web", Type: "http", Target: "http://127.0.0.1", Enabled: true})
	st.UpsertRule(&model.AlertRule{ID: "r1", Name: "CPU", Metric: "cpu", Op: "gt", Threshold: 90, Enabled: true})
	st.SetSetting("theme", `{"mode":"dark"}`)
	h := New(st, nil, nil, Config{AdminToken: testAdminToken}).Handler()

	w := adminReq(h, http.MethodPost, "/api/admin/backup", `{"password":"backup-pass-123"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("导出备份失败: %d: %s", w.Code, w.Body.String())
	}
	var backupResp struct {
		Filename string `json:"filename"`
		Data     string `json:"data"`
		Size     int    `json:"size"`
		Checksum string `json:"checksum"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &backupResp); err != nil {
		t.Fatal(err)
	}
	if backupResp.Data == "" || backupResp.Size == 0 || !strings.HasPrefix(backupResp.Filename, "rollcall-backup-") {
		t.Fatalf("备份响应字段错误: %+v", backupResp)
	}

	body, _ := json.Marshal(map[string]any{
		"password": "backup-pass-123", "data": backupResp.Data, "confirm": true,
	})
	w = adminReq(h, http.MethodPost, "/api/admin/restore", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("导入备份失败: %d: %s", w.Code, w.Body.String())
	}
	var restored struct {
		Nodes    int `json:"nodes"`
		Services int `json:"services"`
		Rules    int `json:"rules"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Nodes != 1 || restored.Services != 1 || restored.Rules != 1 {
		t.Errorf("导入数量错误: %+v", restored)
	}
	if n, ok := st.Node("n1"); !ok || n.Token != "report-token-abc" || n.Country != "JP" {
		t.Errorf("导入后节点信息/令牌丢失: %+v", n)
	}

	// 错误密码与未确认都必须拒绝。
	bad, _ := json.Marshal(map[string]any{
		"password": "wrong-pass-123", "data": backupResp.Data, "confirm": true,
	})
	if w := adminReq(h, http.MethodPost, "/api/admin/restore", string(bad)); w.Code != http.StatusBadRequest {
		t.Errorf("错误密码应 400，实际 %d", w.Code)
	}
	noConfirm, _ := json.Marshal(map[string]any{
		"password": "backup-pass-123", "data": backupResp.Data,
	})
	if w := adminReq(h, http.MethodPost, "/api/admin/restore", string(noConfirm)); w.Code != http.StatusBadRequest {
		t.Errorf("未确认应 400，实际 %d", w.Code)
	}
}

func TestCompareFavoritesAndRates(t *testing.T) {
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "fav", Name: "收藏节点", Group: "prod", Favorite: true, Price: 12, Currency: "USD"})
	st.UpsertNode(&model.Node{ID: "n2", Name: "普通节点", Group: "test", Price: 72, Currency: "CNY"})
	now := time.Now().Unix()
	for _, id := range []string{"fav", "n2"} {
		st.AddMetrics(id, model.HostInfo{}, model.Metrics{TS: now - 60, NetRx: 0, NetTx: 0})
	}
	st.AddMetrics("fav", model.HostInfo{}, model.Metrics{
		TS: now, NetRx: 6000, NetTx: 1200, CPUPct: 20,
		MemUsed: 50, MemTotal: 100, DiskUsed: 25, DiskTotal: 100,
	})
	st.AddMetrics("n2", model.HostInfo{}, model.Metrics{
		TS: now, NetRx: 3000, NetTx: 600, CPUPct: 40,
		MemUsed: 80, MemTotal: 100, DiskUsed: 50, DiskTotal: 100,
	})

	h := New(st, nil, nil, Config{AdminToken: testAdminToken}).Handler()
	w := adminReq(h, http.MethodGet, "/api/compare?favorite=1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("对比接口失败: %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Nodes   []compareNode  `json:"nodes"`
		Summary compareSummary `json:"summary"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Nodes) != 1 || resp.Nodes[0].ID != "fav" {
		t.Fatalf("favorite 过滤失败: %+v", resp.Nodes)
	}
	// 6000 字节 / 60 秒 = 100 B/s，不能用累计计数。
	if resp.Nodes[0].NetRx != 100 || resp.Nodes[0].NetTx != 20 {
		t.Errorf("实时速率计算错误: rx=%d tx=%d", resp.Nodes[0].NetRx, resp.Nodes[0].NetTx)
	}
	if resp.Summary.Nodes != 1 || resp.Summary.Favorites != 1 || resp.Summary.Online != 1 {
		t.Errorf("汇总统计错误: %+v", resp.Summary)
	}

	// 分组过滤与全量汇总。
	w = adminReq(h, http.MethodGet, "/api/compare?group=test", "")
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Nodes) != 1 || resp.Nodes[0].ID != "n2" {
		t.Errorf("group 过滤失败: %+v", resp.Nodes)
	}
	w = adminReq(h, http.MethodGet, "/api/compare", "")
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Nodes) != 2 || resp.Nodes[0].ID != "fav" {
		t.Errorf("收藏节点应排在前面: %+v", resp.Nodes)
	}
}

// testTOTPCode 在测试侧按 RFC 6238 生成动态码。
func testTOTPCode(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).
		DecodeString(strings.ToUpper(strings.ReplaceAll(secret, " ", "")))
	if err != nil {
		t.Fatalf("解码 TOTP 密钥失败: %v", err)
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(at.Unix()/30))
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := (uint32(sum[off])&0x7f)<<24 |
		uint32(sum[off+1])<<16 |
		uint32(sum[off+2])<<8 |
		uint32(sum[off+3])
	return fmt.Sprintf("%06d", code%1_000_000)
}

// TestLoginRequiresTOTP 覆盖密码通过后的两步验证步骤。
func TestLoginRequiresTOTP(t *testing.T) {
	h, _, u := newAuthWebAt(t, time.Hour)
	secret, err := auth.NewTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	// 直接改内存中的账号（服务端持有同一指针），避免测试依赖落盘路径。
	u.TOTPSecret = secret
	u.TOTPEnabled = true

	w := loginReq(h, "10.20.0.1", "demo-admin", testAdminPassword)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("缺少动态码应 401，实际 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"totpRequired":true`) {
		t.Errorf("响应应提示需要动态码: %s", w.Body.String())
	}

	valid := testTOTPCode(t, secret, time.Now())
	wrong := "000000"
	if wrong == valid {
		wrong = "111111"
	}
	w = doAuthReq(h, http.MethodPost, "/api/auth/login", "10.20.0.2",
		`{"identifier":"demo-admin","password":`+mustJSON(testAdminPassword)+`,"totp":`+strconv.Quote(wrong)+`}`,
		map[string]string{"Content-Type": "application/json"}, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("错误动态码应 401，实际 %d", w.Code)
	}

	w = doAuthReq(h, http.MethodPost, "/api/auth/login", "10.20.0.3",
		`{"identifier":"demo-admin","password":`+mustJSON(testAdminPassword)+`,"totp":`+strconv.Quote(valid)+`}`,
		map[string]string{"Content-Type": "application/json"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("正确动态码应登录成功，实际 %d: %s", w.Code, w.Body.String())
	}
	if findSessionCookie(t, w) == nil {
		t.Fatal("登录成功应下发会话 Cookie")
	}
}

// TestTestNotifyUsesTemplates 是回归测试：测试通知曾只复制通道而丢掉
// 已保存的模板，用户点「发送测试通知」看到的仍是默认标题/正文。
func TestTestNotifyUsesTemplates(t *testing.T) {
	notify.SetAllowPrivateTargets(true)
	t.Cleanup(func() { notify.SetAllowPrivateTargets(false) })

	got := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			t.Errorf("解析 webhook 请求: %v", err)
		}
		got <- m
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	st := store.New(t.TempDir())
	cfg := fmt.Sprintf(`{"enabled":true,"minSeverity":"info","titleFormat":"[T] {title}","bodyFormat":"{detail}|{node}|{rule}","channels":[{"id":"wh","type":"webhook","enabled":true,"options":{"url":%q}}]}`, srv.URL)
	st.SetSetting("notify", cfg)

	h := New(st, nil, nil, Config{AdminToken: testAdminToken}).Handler()
	w := adminReq(h, http.MethodPost, "/api/admin/test-notify", `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("测试通知应 200，实际 %d: %s", w.Code, w.Body.String())
	}

	select {
	case m := <-got:
		if m["title"] != "[T] 测试通知" {
			t.Errorf("测试通知标题未应用模板: %v", m["title"])
		}
		if m["body"] != "这是一条来自 Rollcall 的测试通知，收到即表示通道配置正确。|Rollcall 控制端|连通性自检" {
			t.Errorf("测试通知正文未应用模板: %v", m["body"])
		}
	default:
		t.Fatal("Webhook 未收到测试通知")
	}
}

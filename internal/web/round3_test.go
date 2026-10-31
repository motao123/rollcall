package web

import (
	"bytes"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/store"
)

// TestAuthGuardDoesNotClearOnRedirect 是回归测试。
//
// 清零条件曾经是 status < 400，把 3xx 也当成「鉴权成功」。
// 攻击者只要能让受保护路由返回重定向（例如登录跳转），
// 每试 9 次就能重置一次失败计数，限速形同虚设。
func TestAuthGuardDoesNotClearOnRedirect(t *testing.T) {
	s := &Server{limiter: newAuthFailureLimiter(), authUser: "admin", http: newHTTPMetrics()}
	routeOf := func(*http.Request) (authLevel, string) { return authAdmin, "/api/admin/nodes" }

	step := 0
	h := s.withAuthGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		step++
		if step%3 == 0 {
			w.Header().Set("Location", "/login")
			w.WriteHeader(http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}), routeOf)

	req := func() int {
		r := httptest.NewRequest("GET", "/api/admin/nodes", nil)
		r.RemoteAddr = "203.0.113.77:12345"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}

	// 每 3 次里夹一个 302：如果 3xx 会清零，就永远到不了 429。
	for i := 0; i < 30; i++ {
		req()
	}
	if code := req(); code != http.StatusTooManyRequests {
		t.Fatalf("3xx 不应清零失败计数，实际返回 %d", code)
	}
}

// TestAcceptsGzipRejectsAllZeroForms 是回归测试。
// 只比较字面量 "0" 会漏掉 0.0 / 0.00 / 0.000 这几种等价写法。
func TestAcceptsGzipRejectsAllZeroForms(t *testing.T) {
	no := []string{"gzip;q=0", "gzip;q=0.0", "gzip;q=0.00", "gzip; q=0.000", "x-gzip;q=0.0"}
	for _, h := range no {
		if acceptsGzip(h) {
			t.Errorf("%q 应视为拒绝 gzip", h)
		}
	}
	yes := []string{"gzip", "gzip;q=1", "gzip;q=0.5", "deflate, gzip;q=0.8"}
	for _, h := range yes {
		if !acceptsGzip(h) {
			t.Errorf("%q 应接受 gzip", h)
		}
	}
}

// TestContentDispositionHandlesNonASCII 防止中文节点名产生乱码文件名。
// 节点名普遍含非 ASCII 字符（中文昵称最常见），而 filename= 只允许 ASCII。
func TestContentDispositionHandlesNonASCII(t *testing.T) {
	got := contentDisposition("rollcall-metrics-演示节点-20261018-2230.csv")
	if !strings.Contains(got, "filename*=UTF-8''") {
		t.Fatalf("缺少 RFC 5987 形式: %q", got)
	}
	if !strings.Contains(got, "%E6%BC%94%E7%A4%BA") { // 演示
		t.Fatalf("中文未被百分号编码: %q", got)
	}
	// ASCII 回退段必须不含非 ASCII 字节
	i := strings.Index(got, "filename=")
	j := strings.Index(got, "filename*=")
	if i < 0 || j <= i {
		t.Fatalf("头部格式异常: %q", got)
	}
	fallback := got[i:j]
	for _, r := range fallback {
		if r > 0x7e {
			t.Fatalf("ASCII 回退段含非 ASCII 字符: %q", fallback)
		}
	}
	// 引号必须被替换，否则会提前闭合头字段
	if strings.Contains(contentDisposition(`a"b.csv`), `"a"b.csv"`) {
		t.Fatal("文件名中的引号未转义")
	}
}

// TestSilenceRejectsTrailingJSON 是回归测试。
// json.Decoder 只消费第一个值，`{"minutes":60}{"minutes":43200}`
// 会被静默当成前者，调用方以为保存了 30 天静默。
func TestSilenceRejectsTrailingJSON(t *testing.T) {
	st := store.New(t.TempDir())
	const adminTok = "test-admin-token-1234567890"
	srv := New(st, nil, nil, Config{AdminToken: adminTok})
	h := srv.Handler()

	body := []byte(`{"minutes":60,"reason":"a"}{"minutes":43200,"reason":"b"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/silences/global", bytes.NewReader(body))
	req.Header.Set("X-Admin-Token", adminTok)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("尾随 JSON 应返回 400，实际 %d: %s", w.Code, w.Body.String())
	}

	// 正常单值仍然可用
	body = []byte(`{"minutes":60,"reason":"维护"}`)
	req = httptest.NewRequest(http.MethodPost, "/api/admin/silences/global", bytes.NewReader(body))
	req.Header.Set("X-Admin-Token", adminTok)
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("正常请求应 200，实际 %d: %s", w.Code, w.Body.String())
	}
}

// TestSanitizeMetricsClampsBadInput 是回归测试。
//
// Agent 是可能被攻破的一端。NaN/Inf 无法被 JSON 编码，会让整份
// history.json 写不出去（Save 持续失败）；用量大于总量会让 UI 出现
// >100% 或负剩余量。
func TestSanitizeMetricsClampsBadInput(t *testing.T) {
	m := sanitizeMetrics(model.Metrics{
		CPUPct:   math.NaN(),
		Load1:    -3,
		Load5:    math.Inf(1),
		MemUsed:  100,
		MemTotal: 50,
		DiskUsed: 10,
		// DiskTotal = 0
		SwapUsed: 5,
		TCPConn:  -1,
		Procs:    -2,
	})
	if m.CPUPct != 0 {
		t.Errorf("NaN CPU 应归零，实际 %v", m.CPUPct)
	}
	if m.Load1 != 0 || m.Load5 != 1e6 {
		t.Errorf("负载未被夹取: %v %v", m.Load1, m.Load5)
	}
	if m.MemUsed != 50 {
		t.Errorf("用量应夹到总量，实际 %v", m.MemUsed)
	}
	if m.DiskUsed != 0 {
		t.Errorf("总量为 0 时用量应归零，实际 %v", m.DiskUsed)
	}
	if m.SwapUsed != 0 {
		t.Errorf("Swap 总量为 0 时用量应归零，实际 %v", m.SwapUsed)
	}
	if m.TCPConn < 0 || m.Procs < 0 {
		t.Errorf("负数计数应归零: %+v", m)
	}
}

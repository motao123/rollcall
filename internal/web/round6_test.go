package web

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/motao/rollcall/internal/store"
)

// TestStatusPageHasCSSOnlyScriptFallback 是回归测试。
//
// CSP 是 script-src 'self'，不能内联脚本做「脚本没加载」的检测，
// 于是脚本被扩展拦截或返回 404 时，骨架屏会永远脉动，用户以为页面卡死。
// 现在用纯 CSS 在 12 秒后让骨架退场并揭示提示。
func TestStatusPageHasCSSOnlyScriptFallback(t *testing.T) {
	if !strings.Contains(statusPageHTML, `id="jsfail"`) {
		t.Fatal("状态页缺少脚本加载失败的兜底提示")
	}
	if !strings.Contains(statusPageHTML, "skelOut") {
		t.Fatal("骨架屏缺少超时退场动画（纯 CSS 兜底）")
	}
	if strings.Contains(statusPageHTML, "<script>") {
		t.Fatal("兜底不能引入内联脚本（CSP script-src 'self'）")
	}
}

// TestStatusPageServed 确认页面能正常返回并带 CSP。
func TestStatusPageServed(t *testing.T) {
	srv := New(store.New(t.TempDir()), nil, nil, Config{})
	req := httptest.NewRequest("GET", "/status", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("/status = %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("CSP 头异常: %q", rec.Header().Get("Content-Security-Policy"))
	}
	if !strings.Contains(rec.Body.String(), `id="jsfail"`) {
		t.Fatal("实际响应缺少兜底提示")
	}
}

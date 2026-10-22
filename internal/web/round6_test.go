package web

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStatusPageHasCSSOnlyScriptFallback 是回归测试。
//
// CSP 是 script-src 'self'，不能内联脚本做「脚本没加载」的检测，
// 于是脚本被扩展拦截或返回 404 时，骨架屏会永远脉动，用户以为页面卡死。
// 现在用纯 CSS 在 12 秒后让骨架退场并揭示提示；脚本正常执行时
// 由 status.js 给 <html> 加 js-ok 关掉提示，避免成功加载后误报。
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
	// display:none 在部分浏览器中无法通过 keyframes 变为可见，
	// 兜底提示会永远不出现；必须用可动画的属性并配合 js-ok 关闭。
	if strings.Contains(statusPageHTML, "@keyframes jsfailIn{to{display:block}}") {
		t.Fatal("兜底提示不能依赖 display 动画")
	}
	if !strings.Contains(statusPageHTML, "html.js-ok #jsfail") {
		t.Fatal("缺少 js-ok 关闭兜底提示的规则（脚本成功后会误报）")
	}
	if !strings.Contains(statusPageHTML, "jsfailIn ") || !strings.Contains(statusPageHTML, "visibility") {
		t.Fatal("兜底提示应使用 visibility/opacity 动画")
	}
}

// TestStatusPageServed 确认页面能正常返回并带 CSP。
func TestStatusPageServed(t *testing.T) {
	srv := New(publicStore(t), nil, nil, Config{})
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

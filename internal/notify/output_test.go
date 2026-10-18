package notify

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPostJSONTrimsURL 防止「校验通过但请求失败」。
//
// checkNotifyURL 内部做了 TrimSpace，但真正的 http.NewRequest 用的是原串；
// 用户从浏览器粘贴地址时首尾带空格很常见。
func TestPostJSONTrimsURL(t *testing.T) {
	SetAllowPrivateTargets(true)
	t.Cleanup(func() { SetAllowPrivateTargets(false) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	if _, err := postJSON("  "+srv.URL+"  ", map[string]string{"a": "b"}); err != nil {
		t.Fatalf("首尾空格应被裁掉: %v", err)
	}
}

// TestSanitizePluginOutput 防止插件输出把控制字符/多行内部信息带进 API 响应。
func TestSanitizePluginOutput(t *testing.T) {
	in := "\x1b[31mfailed\x1b[0m\nsecret=abc\tinternal"
	out := sanitizePluginOutput(in)
	if strings.ContainsAny(out, "\r\n\t") {
		t.Fatalf("换行/制表符应被压平: %q", out)
	}
	if strings.ContainsRune(out, 0x1b) {
		t.Fatalf("ANSI 转义应被去除: %q", out)
	}
	if len([]rune(out)) > 161 {
		t.Fatalf("输出应被截断，实际 %d 字符", len([]rune(out)))
	}
}

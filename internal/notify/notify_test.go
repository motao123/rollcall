package notify

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDecodeResultRejectsNonJSON(t *testing.T) {
	var v struct{ Code int }
	if err := decodeResult([]byte("ok"), &v); err == nil {
		t.Fatal("非 JSON 响应应报错，不能当作成功")
	}
	if err := decodeResult(nil, &v); err == nil {
		t.Fatal("空响应应报错")
	}
	if err := decodeResult([]byte(`{"code":1}`), &v); err != nil || v.Code != 1 {
		t.Fatalf("合法 JSON 解析失败: %v %+v", err, v)
	}
}

func TestCheckNotifyURL(t *testing.T) {
	SetAllowPrivateTargets(false)
	t.Cleanup(func() { SetAllowPrivateTargets(false) })

	if err := checkNotifyURL("http://169.254.169.254/latest/meta-data/"); err == nil {
		t.Error("metadata 地址必须被拒绝")
	}
	if err := checkNotifyURL("file:///etc/passwd"); err == nil {
		t.Error("非 http/https 协议必须被拒绝")
	}
	if err := checkNotifyURL("http://192.168.1.10:8080/hook"); err == nil {
		t.Error("默认应拒绝私网通知地址")
	}

	SetAllowPrivateTargets(true)
	if err := checkNotifyURL("http://192.168.1.10:8080/hook"); err != nil {
		t.Errorf("显式允许后私网通知地址应通过: %v", err)
	}
	if err := checkNotifyURL("http://169.254.169.254/latest/meta-data/"); err == nil {
		t.Error("即使允许私网，metadata 地址也必须被拒绝")
	}
}

func TestValidateRejectsBadChannels(t *testing.T) {
	cfg := &Config{Channels: []ChannelConfig{{ID: "a", Type: "banana"}}}
	if err := cfg.Validate(); err == nil {
		t.Error("未知通道类型应被拒绝")
	}
	cfg = &Config{Channels: []ChannelConfig{
		{ID: "dup", Type: "webhook"}, {ID: "dup", Type: "webhook"},
	}}
	if err := cfg.Validate(); err == nil {
		t.Error("重复通道 ID 应被拒绝")
	}
	cfg = &Config{Channels: []ChannelConfig{
		{ID: "a", Type: "webhook", Options: map[string]string{"url": "https://example.com/hook"}},
	}}
	if err := cfg.Validate(); err != nil {
		t.Errorf("合法配置不应报错: %v", err)
	}
}

func TestEmailRequiresSender(t *testing.T) {
	ch := ChannelConfig{Type: "email", Options: map[string]string{
		"smtpHost": "smtp.example.com", "to": "rcpt@example.com",
	}}
	if got := missingRequired(ch); got == "" {
		t.Error("既无 from 也无 username 时邮箱通道应视为配置不完整")
	}
	ch.Options["username"] = "sender@example.com"
	if got := missingRequired(ch); got != "" {
		t.Errorf("username 可回退为发件人，不应报缺: %q", got)
	}
}

func TestNetErrStripsURL(t *testing.T) {
	raw := "https://api.telegram.org/bot123456:secret-token/sendMessage"
	inner := errors.New("connection refused")
	err := netErr(&url.Error{Op: "Post", URL: raw, Err: inner})
	if err == nil {
		t.Fatal("应返回错误")
	}
	if strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), raw) {
		t.Errorf("错误信息不应包含完整 URL: %v", err)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("应保留底层错误: %v", err)
	}
}

// TestPostJSONDoesNotFollowRedirects 防止令牌/消息体被 3xx 转发到其他主机。
func TestPostJSONDoesNotFollowRedirects(t *testing.T) {
	SetAllowPrivateTargets(true)
	t.Cleanup(func() { SetAllowPrivateTargets(false) })

	var hits int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirector.Close()

	_, err := postJSON(redirector.URL, map[string]string{"x": "1"})
	if err == nil {
		t.Fatal("302 应作为失败返回")
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatal("不应跟随重定向请求第二个地址")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("错误信息不应包含远端响应体: %v", err)
	}
}

func TestServerchanRejectsURLLikeKey(t *testing.T) {
	err := sendServerchan(map[string]string{"sendKey": "http://evil.example/hook"}, "s", "b")
	if err == nil || !strings.Contains(err.Error(), "格式无效") {
		t.Fatalf("完整 URL 作为 sendKey 应被拒绝: %v", err)
	}
}

func TestEmailHeaderInjectionRejected(t *testing.T) {
	base := map[string]string{
		"smtpHost": "127.0.0.1", "smtpPort": "1025",
		"from": "sender@example.com", "to": "rcpt@example.com",
	}
	for _, opt := range []map[string]string{
		{"from": "a@example.com\r\nBcc: victim@example.com"},
		{"from": "a@example.com", "to": "rcpt@example.com\r\nBcc: victim@example.com"},
	} {
		o := map[string]string{}
		for k, v := range base {
			o[k] = v
		}
		for k, v := range opt {
			o[k] = v
		}
		if err := sendEmail(o, "subject", "body"); err == nil || !strings.Contains(err.Error(), "无效") {
			// 解析失败应发生在任何网络连接之前
			t.Errorf("含 CRLF 的地址应被拒绝，err=%v", err)
		}
	}
}

func TestEncodeHeaderStripsCRLF(t *testing.T) {
	for _, in := range []string{"hello\r\nBcc: x@example.com", "中文\r\n注入"} {
		out := encodeHeader(in)
		if strings.ContainsAny(out, "\r\n") {
			t.Errorf("encodeHeader(%q) 仍含 CR/LF: %q", in, out)
		}
	}
}

func TestInvalidMinSeverityFallsBackToWarn(t *testing.T) {
	cfg, err := Parse(`{"enabled":true,"minSeverity":"banana","repeatInterval":-5}`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MinSeverity != "warn" {
		t.Errorf("非法阈值应回退为 warn，实际 %q", cfg.MinSeverity)
	}
	if cfg.RepeatInterval != 0 {
		t.Errorf("负的重复间隔应归零，实际 %d", cfg.RepeatInterval)
	}
	if !cfg.ShouldSend("warn") || cfg.ShouldSend("info") {
		t.Error("warn 阈值过滤不正确")
	}
	if cfg.ShouldSend("unknown") {
		t.Error("未知严重度不应通过过滤")
	}
}

package probe

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/motao/rollcall/internal/model"
)

func TestParseTCPTarget(t *testing.T) {
	ok := []struct{ in, want string }{
		{"example.com:443", "example.com:443"},
		{"tcp://example.com:80", "example.com:80"},
		{"1.2.3.4:65535", "1.2.3.4:65535"},
		{"[::1]:443", "[::1]:443"},
	}
	for _, c := range ok {
		got, err := parseTCPTarget(c.in)
		if err != nil || got != c.want {
			t.Errorf("parseTCPTarget(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	for _, in := range []string{
		"example.com",           // 缺端口
		"example.com:0",         // 端口越界
		"example.com:70000",     // 端口越界
		"http://example.com:80", // 非 tcp scheme 会含冒号被当作 host:port? 应拒绝
		"example.com:80/path",   // 含路径
	} {
		if _, err := parseTCPTarget(in); err == nil {
			t.Errorf("parseTCPTarget(%q) 应当报错", in)
		}
	}
}

func TestValidateServiceBlocksPrivateByDefault(t *testing.T) {
	r := NewRunner(Config{})
	for _, sv := range []model.Service{
		{Type: "http", Target: "http://127.0.0.1:8080/"},
		{Type: "http", Target: "http://169.254.169.254/latest/meta-data/"},
		{Type: "tcp", Target: "10.0.0.5:22"},
	} {
		if err := r.ValidateService(sv); err == nil {
			t.Errorf("默认策略应拒绝内网目标 %s", sv.Target)
		}
	}
	if err := r.ValidateService(model.Service{Type: "http", Target: "https://example.com/health"}); err != nil {
		t.Errorf("公网目标不应被拒绝: %v", err)
	}
	if err := r.ValidateService(model.Service{Type: "http", Target: "ftp://example.com/"}); err == nil {
		t.Error("非 http/https 协议应被拒绝")
	}
	if err := r.ValidateService(model.Service{Type: "icmp", Target: "example.com"}); err == nil {
		t.Error("不支持的类型应被拒绝")
	}
}

// TestHTTPProbeStatusSemantics 校验健康判定：仅 2xx 与 401/403 为健康，
// 3xx/5xx 不再默认视为在线。
func TestHTTPProbeStatusSemantics(t *testing.T) {
	r := NewRunner(Config{AllowPrivate: true})
	cases := []struct {
		code int
		ok   bool
	}{
		{200, true}, {204, true}, {401, true}, {403, true},
		{301, false}, {404, false}, {500, false},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.WriteHeader(c.code)
		}))
		res := r.Do(model.Service{Type: "http", Target: srv.URL})
		srv.Close()
		if res.OK != c.ok {
			t.Errorf("status %d: OK=%v detail=%q, want %v", c.code, res.OK, res.Detail, c.ok)
		}
	}
}

// TestHTTPProbeBlocksRedirectToMetadata 是 SSRF 回归测试：
// 公网地址 302 到 169.254.169.254 时，必须在发请求前拒绝。
func TestHTTPProbeBlocksRedirectToMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()

	r := NewRunner(Config{AllowPrivate: true})
	res := r.Do(model.Service{Type: "http", Target: srv.URL})
	if res.OK {
		t.Fatalf("跳转到 metadata 地址不应判定为健康: %+v", res)
	}
	if !strings.Contains(res.Detail, "禁止访问") {
		t.Errorf("错误信息应说明地址被禁止: %q", res.Detail)
	}
}

func TestTCPProbe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	r := NewRunner(Config{AllowPrivate: true})
	res := r.Do(model.Service{Type: "tcp", Target: ln.Addr().String()})
	if !res.OK {
		t.Fatalf("本机监听端口探测失败: %+v", res)
	}

	blocked := NewRunner(Config{}).Do(model.Service{Type: "tcp", Target: ln.Addr().String()})
	if blocked.OK {
		t.Fatalf("默认策略不应允许探测回环 TCP: %+v", blocked)
	}
}

// TestCertDaysLeft 固定 0/-1 语义：0=已过期，>0=剩余天数（不足一天按 1 计）。
func TestCertDaysLeft(t *testing.T) {
	if got := certDaysLeft(time.Now().Add(-2 * time.Hour)); got != 0 {
		t.Errorf("过期证书应为 0，实际 %d", got)
	}
	if got := certDaysLeft(time.Now().Add(30 * time.Minute)); got != 1 {
		t.Errorf("不足一天应按 1 计，实际 %d", got)
	}
	if got := certDaysLeft(time.Now().Add(25 * time.Hour)); got != 2 {
		t.Errorf("25 小时应向上取整为 2，实际 %d", got)
	}
	if got := certDaysLeft(time.Now().Add(30 * 24 * time.Hour)); got != 30 {
		t.Errorf("30 天证书应显示 30，实际 %d", got)
	}
}

// TestTrimErrUnwrapsURLAndKeepsCause 回归测试：
// 不能把 `Get "https://host/": dial tcp ...` 截成只剩 URL，也不能切坏中文。
func TestTrimErrUnwrapsURLAndKeepsCause(t *testing.T) {
	err := &url.Error{Op: "Get", URL: "https://example.com/", Err: errors.New("dial tcp 1.2.3.4:443: i/o timeout")}
	got := trimErr(err)
	if strings.Contains(got, "https://example.com/") {
		t.Errorf("应解包 url.Error，实际 %q", got)
	}
	if !strings.Contains(got, "i/o timeout") {
		t.Errorf("应保留底层原因，实际 %q", got)
	}
	long := trimErr(errors.New(strings.Repeat("超时", 100)))
	if !utf8.ValidString(long) {
		t.Errorf("截断后不是合法 UTF-8: %q", long)
	}
}

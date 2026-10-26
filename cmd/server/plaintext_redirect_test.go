package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHTTPSURL 覆盖跳转目标的拼装规则。
func TestHTTPSURL(t *testing.T) {
	cases := []struct {
		reqLine string
		host    string
		want    string
		ok      bool
	}{
		{"GET /status HTTP/1.1", "1.2.3.4:19080", "https://1.2.3.4:19080/status", true},
		{"HEAD / HTTP/1.1", "mon.example.com", "https://mon.example.com/", true},
		// 80 端口不能带进 https 地址
		{"GET /status#/n/x HTTP/1.1", "mon.example.com:80", "https://mon.example.com/status#/n/x", true},
		// 绝对形式请求行只取路径
		{"GET http://mon.example.com/api/status HTTP/1.1", "mon.example.com", "https://mon.example.com/api/status", true},
		// 没有 Host 无法构造：宁可不跳
		{"GET /status HTTP/1.1", "", "", false},
		// POST 跳转会丢请求体，宁可让客户端看到 400
		{"POST /api/admin/nodes HTTP/1.1", "mon.example.com", "", false},
		{"garbage", "mon.example.com", "", false},
	}
	for _, c := range cases {
		got, ok := httpsURL(c.reqLine, c.host)
		if ok != c.ok || got != c.want {
			t.Errorf("httpsURL(%q,%q) = %q,%v；期望 %q,%v", c.reqLine, c.host, got, ok, c.want, c.ok)
		}
	}
}

// TestSniffListenerKeepsTLSWorking 确认加了窥探之后 TLS 依然正常，
// 这是最关键的回归点：判断错首字节会把所有 TLS 连接打断。
func TestSniffListenerKeepsTLSWorking(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok-tls")
	}))
	srv.Listener = &sniffListener{Listener: srv.Listener}
	srv.StartTLS()
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("TLS 请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "ok-tls" {
		t.Fatalf("TLS 请求异常: %d %q", resp.StatusCode, body)
	}
}

// TestSniffListenerRedirectsPlaintext 是这次改动的目的：
// http:// 访问要 301 到 https，而不是回一行让人看不懂的 400。
func TestSniffListenerRedirectsPlaintext(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok-tls")
	}))
	srv.Listener = &sniffListener{Listener: srv.Listener}
	srv.StartTLS()
	defer srv.Close()

	addr := srv.Listener.Addr().String()
	// 不跟随跳转：要看到 301 本身
	cli := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := cli.Get("http://" + addr + "/status#/n/abc")
	if err != nil {
		t.Fatalf("明文请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("明文访问应 301，实际 %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc != "https://"+addr+"/status" {
		t.Fatalf("跳转目标不对: %q", loc)
	}
	// 片段（#/n/abc）不在 HTTP 请求行里 —— 浏览器不会发给服务端，
	// 跳转后它自己保留。所以这里断言的是"路径原样带过去"。

	// 同一个 listener 在明文请求之后必须还能服务 TLS
	resp2, err := srv.Client().Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("明文请求之后 TLS 仍应可用: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("明文请求之后 TLS 返回 %d", resp2.StatusCode)
	}
}

// TestSniffListenerRejectsNonHTTP 确认乱发的字节不会让监听卡住或崩掉。
func TestSniffListenerRejectsNonHTTP(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Listener = &sniffListener{Listener: srv.Listener}
	srv.StartTLS()
	defer srv.Close()

	addr := srv.Listener.Addr().String()
	cli := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	// POST 走明文：按设计不跳转，回 400
	resp, err := cli.Post("http://"+addr+"/api/x", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("明文 POST 失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("明文 POST 应 400（不跳转以免丢请求体），实际 %d", resp.StatusCode)
	}

	if resp2, err := srv.Client().Get(srv.URL + "/"); err != nil || resp2.StatusCode != 200 {
		t.Fatalf("异常请求之后 TLS 应仍可用: %v", err)
	}
}

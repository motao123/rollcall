package web

import (
	"net/http/httptest"
	"testing"
)

// TestClientIPRespectsProxyChain 防止反代后全站被折叠成同一个 IP。
//
// 反向代理（nginx/隧道）终止 TLS 时，所有客户端共享代理地址；
// 若只看 RemoteAddr，任意人发 10 次错误凭证就能让整站 429 十分钟。
// 但也不能无条件信任 X-Forwarded-For —— 公网直连的客户端可以伪造它来绕过限速。
func TestClientIPRespectsProxyChain(t *testing.T) {
	cases := []struct {
		name   string
		remote string
		xff    string
		want   string
	}{
		{"本机代理取 XFF", "127.0.0.1:5555", "203.0.113.9", "203.0.113.9"},
		{"内网代理取 XFF", "192.168.1.10:5555", "198.51.100.7", "198.51.100.7"},
		{"代理链取最右非可信地址", "127.0.0.1:5555", "1.1.1.1, 203.0.113.9", "203.0.113.9"},
		{"公网直连忽略 XFF", "203.0.113.50:5555", "1.1.1.1", "203.0.113.50"},
		{"无 XFF 回退 RemoteAddr", "127.0.0.1:5555", "", "127.0.0.1"},
		{"IPv6 本机代理", "[::1]:5555", "203.0.113.9", "203.0.113.9"},
		{"XFF 带端口", "127.0.0.1:5555", "203.0.113.9:443", "203.0.113.9"},
		{"XFF 全为内网时回退代理地址", "127.0.0.1:5555", "10.0.0.2", "127.0.0.1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = c.remote
			if c.xff != "" {
				r.Header.Set("X-Forwarded-For", c.xff)
			}
			if got := clientIP(r); got != c.want {
				t.Errorf("remote=%s xff=%q got=%s want=%s", c.remote, c.xff, got, c.want)
			}
		})
	}
}

package web

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func mustNets(t *testing.T, s string) []*net.IPNet {
	t.Helper()
	n, err := ParseTrustedProxies(s)
	if err != nil {
		t.Fatalf("解析 %q: %v", s, err)
	}
	return n
}

// TestClientIPRespectsProxyChain 防止反代后全站被折叠成同一个 IP，
// 同时防止「私网即可信」让同 VPC 内任意主机伪造 XFF 绕过限速。
func TestClientIPRespectsProxyChain(t *testing.T) {
	lan := mustNets(t, "192.168.1.0/24")
	lanAndLoopback := mustNets(t, "127.0.0.1/32,10.0.0.0/8")
	cases := []struct {
		name    string
		remote  string
		xff     string
		trusted []*net.IPNet
		want    string
	}{
		{"回环代理取 XFF", "127.0.0.1:5555", "203.0.113.9", defaultTrustedProxies, "203.0.113.9"},
		{"内网但不在白名单 → 忽略 XFF", "192.168.1.10:5555", "198.51.100.7", defaultTrustedProxies, "192.168.1.10"},
		{"显式白名单内的内网代理", "192.168.1.10:5555", "198.51.100.7", lan, "198.51.100.7"},
		{"代理链取最右非可信地址", "127.0.0.1:5555", "1.1.1.1, 203.0.113.9", defaultTrustedProxies, "203.0.113.9"},
		{"公网直连忽略 XFF", "203.0.113.50:5555", "1.1.1.1", defaultTrustedProxies, "203.0.113.50"},
		{"无 XFF 回退 RemoteAddr", "127.0.0.1:5555", "", defaultTrustedProxies, "127.0.0.1"},
		{"IPv6 回环代理", "[::1]:5555", "203.0.113.9", defaultTrustedProxies, "203.0.113.9"},
		{"XFF 带端口", "127.0.0.1:5555", "203.0.113.9:443", defaultTrustedProxies, "203.0.113.9"},
		{"XFF 全为可信时回退代理地址", "127.0.0.1:5555", "10.0.0.2", lanAndLoopback, "127.0.0.1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = c.remote
			if c.xff != "" {
				r.Header.Set("X-Forwarded-For", c.xff)
			}
			if got := clientIP(r, c.trusted); got != c.want {
				t.Errorf("remote=%s xff=%q got=%s want=%s", c.remote, c.xff, got, c.want)
			}
		})
	}
}

func TestParseTrustedProxies(t *testing.T) {
	nets, err := ParseTrustedProxies("127.0.0.1/32, 10.0.0.0/8, 2001:db8::1")
	if err != nil {
		t.Fatalf("合法输入不应报错: %v", err)
	}
	if len(nets) != 3 {
		t.Fatalf("应解析出 3 条，实际 %d", len(nets))
	}
	if _, err := ParseTrustedProxies("not-an-ip"); err == nil {
		t.Fatal("非法地址应报错")
	}
	if nets, err := ParseTrustedProxies(""); err != nil || len(nets) != 0 {
		t.Fatalf("空串应得到空列表: %v %d", err, len(nets))
	}
}

// TestClientIPReadsAllXFFHeaderLines 覆盖代理把客户端追加成新 header 行的情况。
func TestClientIPReadsAllXFFHeaderLines(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:5555"
	r.Header.Add("X-Forwarded-For", "1.1.1.1")
	r.Header.Add("X-Forwarded-For", "203.0.113.9")
	if got := clientIP(r, defaultTrustedProxies); got != "203.0.113.9" {
		t.Fatalf("多行 XFF 应取最右侧非可信地址，实际 %s", got)
	}
}

// TestRateLimitKeyFoldsIPv6To64 防止轮换 IPv6 源地址绕过失败限速。
func TestRateLimitKeyFoldsIPv6To64(t *testing.T) {
	a := rateLimitKey("2001:db8:1:2::1")
	b := rateLimitKey("2001:db8:1:2:ffff::9")
	if a != b {
		t.Fatalf("同一 /64 内应折叠为同一 key: %q != %q", a, b)
	}
	if a == rateLimitKey("2001:db8:1:3::1") {
		t.Fatal("不同 /64 不应折叠")
	}
	if got := rateLimitKey("203.0.113.9"); got != "203.0.113.9" {
		t.Fatalf("IPv4 应保持原样，实际 %q", got)
	}
}

// TestLimiterReservesInflightSlots 防止并发请求在 fail() 之前全部放行。
func TestLimiterReservesInflightSlots(t *testing.T) {
	l := newAuthFailureLimiter()
	const reserve = 2 * 10 // maxFails*2
	for i := 0; i < reserve; i++ {
		if ok, _ := l.allow("scope"); !ok {
			t.Fatalf("第 %d 个在途请求不应被拒", i+1)
		}
	}
	if ok, _ := l.allow("scope"); ok {
		t.Fatal("在途名额耗尽后应拒绝新请求")
	}
	l.release("scope")
	if ok, _ := l.allow("scope"); !ok {
		t.Fatal("结算后应重新放行")
	}
}

// TestStatusRecorderIgnoresInformational 防止 1xx 中间响应顶掉最终状态码。
func TestStatusRecorderIgnoresInformational(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
	rec.WriteHeader(http.StatusEarlyHints)
	if rec.status != 0 {
		t.Fatalf("1xx 不应被记为最终状态，实际 %d", rec.status)
	}
	rec.WriteHeader(http.StatusOK)
	if rec.status != http.StatusOK {
		t.Fatalf("最终状态应为 200，实际 %d", rec.status)
	}
}

// TestLimiterSweepsExpiredEntries 验证清扫只删过期且未封禁的条目。
func TestLimiterSweepsExpiredEntries(t *testing.T) {
	l := newAuthFailureLimiter()
	now := time.Now()
	l.state["expired"] = &authFailState{windowStart: now.Add(-2 * l.window)}
	l.state["active"] = &authFailState{windowStart: now}
	l.state["blocked"] = &authFailState{
		windowStart:  now.Add(-2 * l.window),
		blockedUntil: now.Add(time.Minute),
	}

	if ok, _ := l.allow("other"); !ok {
		t.Fatal("未记录的地址应放行")
	}
	if _, ok := l.state["expired"]; ok {
		t.Fatal("过期条目应被清理")
	}
	if _, ok := l.state["active"]; !ok {
		t.Fatal("活跃条目不应被清理")
	}
	if _, ok := l.state["blocked"]; !ok {
		t.Fatal("封禁中的条目不应被清理")
	}
}

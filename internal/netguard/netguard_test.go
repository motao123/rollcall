package netguard

import (
	"net"
	"net/url"
	"testing"
)

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("解析 %q: %v", raw, err)
	}
	return u
}

func TestCheckIPPolicy(t *testing.T) {
	cases := []struct {
		ip           string
		allowPrivate bool
		wantErr      bool
	}{
		{"8.8.8.8", false, false},
		{"8.8.8.8", true, false},
		{"127.0.0.1", false, true},
		{"127.0.0.1", true, false},
		{"10.1.2.3", false, true},
		{"10.1.2.3", true, false},
		{"192.168.1.1", false, true},
		{"172.16.0.1", false, true},
		{"::1", false, true},
		{"fd00::1", false, true},
		{"100.64.0.1", false, true},
		{"100.64.0.1", true, false}, // Tailscale 等 CGNAT 组网
		// 落在 CGNAT / ULA 段内的云元数据地址，即使允许私网也必须拒绝
		{"100.100.100.200", true, true}, // 阿里云 metadata
		{"fd00:ec2::254", true, true},   // AWS IPv6 metadata
		// metadata / 链路本地始终禁止，即使显式允许私网
		{"169.254.169.254", true, true},
		{"fe80::1", true, true},
		{"0.0.0.0", true, true},
		{"224.0.0.1", true, true},
	}
	for _, c := range cases {
		p := Policy{AllowPrivate: c.allowPrivate}
		err := p.CheckIP(net.ParseIP(c.ip))
		if (err != nil) != c.wantErr {
			t.Errorf("CheckIP(%s, allowPrivate=%v) err=%v, wantErr=%v",
				c.ip, c.allowPrivate, err, c.wantErr)
		}
	}
}

func TestCheckURL(t *testing.T) {
	p := Policy{}
	for _, raw := range []string{
		"ftp://example.com/x",
		"file:///etc/passwd",
		"http:///missing-host",
	} {
		u := mustParseURL(t, raw)
		if err := p.CheckURL(u); err == nil {
			t.Errorf("CheckURL(%q) 应当被拒绝", raw)
		}
	}
	if err := p.CheckURL(mustParseURL(t, "https://example.com/hook")); err != nil {
		t.Errorf("公网 https 地址不应被拒绝: %v", err)
	}
}

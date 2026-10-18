package netguard

import (
	"net/url"
	"testing"
)

// TestCheckHostRejectsZoneScopedIPv6 防止带区域后缀的 IPv6 绕过地址策略。
//
// net.ParseIP 不认 zone，`fe80::1%eth0` 会被当成域名返回 nil；
// URL 中该地址写作 `[fe80::1%25eth0]`，Hostname() 解出 `fe80::1%eth0`。
func TestCheckHostRejectsZoneScopedIPv6(t *testing.T) {
	p := Policy{}
	for _, host := range []string{"fe80::1%eth0", "fe80::1%25eth0", "::1%lo"} {
		if err := p.CheckHost(host); err == nil {
			t.Errorf("CheckHost(%q) 应被拒绝", host)
		}
	}
	if err := p.CheckHost("example.com"); err != nil {
		t.Errorf("普通域名不应被拒绝: %v", err)
	}

	u, err := url.Parse("http://[fe80::1%25eth0]/")
	if err != nil {
		t.Fatalf("解析 URL: %v", err)
	}
	if err := p.CheckURL(u); err == nil {
		t.Error("带 zone 的 IPv6 URL 应被拒绝")
	}
}

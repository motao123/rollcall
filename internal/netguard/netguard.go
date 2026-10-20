// Package netguard 提供出站地址策略，防止探测与通知被用于 SSRF。
//
// 默认拒绝回环、私网、CGNAT、链路本地（含云厂商 metadata 169.254.169.254）
// 与保留网段；调用方可以按需放行私网（自建服务监控/自托管通知）。
// 策略在**拨号时**对实际解析出的 IP 生效，因此 DNS rebinding 也无法绕过。
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// Policy 描述一次出站访问允许的地址范围。
type Policy struct {
	// AllowPrivate 放行回环与 RFC1918/ULA 私网地址。
	// 链路本地、metadata、组播、未指定地址始终禁止。
	AllowPrivate bool
	// Timeout 是单次拨号超时，<=0 时使用 8 秒。
	Timeout time.Duration
}

var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// reservedIPv4 是不可路由的保留网段：文档/基准测试网段与 240/4（含广播地址）。
// 它们永远不是真实服务，放行只会产生误导性的「可达」结果，
// 也可能被用于广播/放大探测。
var reservedIPv4 = []*net.IPNet{
	{IP: net.IPv4(192, 0, 2, 0), Mask: net.CIDRMask(24, 32)},    // TEST-NET-1
	{IP: net.IPv4(198, 51, 100, 0), Mask: net.CIDRMask(24, 32)}, // TEST-NET-2
	{IP: net.IPv4(203, 0, 113, 0), Mask: net.CIDRMask(24, 32)},  // TEST-NET-3
	{IP: net.IPv4(198, 18, 0, 0), Mask: net.CIDRMask(15, 32)},   // 基准测试
	{IP: net.IPv4(192, 0, 0, 0), Mask: net.CIDRMask(24, 32)},    // IETF 协议分配
	{IP: net.IPv4(240, 0, 0, 0), Mask: net.CIDRMask(4, 32)},     // 保留 + 广播
}

// metadataIPs 是云厂商元数据服务的固定地址，必须**无条件**拦截。
//
// 169.254.169.254 等链路本地地址已由 IsLinkLocalUnicast 覆盖；
// 以下两个落在 CGNAT / ULA 段内，会被 AllowPrivate 放行，故单独列出：
//   - 100.100.100.200  阿里云（100.64/10 CGNAT）
//   - fd00:ec2::254    AWS IPv6（fc00::/7 ULA）
//
// 元数据服务永远不是合法的「自建内网服务」目标；一旦放行，
// 用户可把通知地址指向它，从而窃取实例凭据。
var metadataIPs = []net.IP{
	net.IPv4(100, 100, 100, 200),
	net.ParseIP("fd00:ec2::254"),
	net.ParseIP("fd20:ce::254"), // GCP IPv6 metadata（落在 ULA 内）
}

// reservedIPv6 是不应访问的 IPv6 保留/过渡网段。
//
// 其中 Teredo / 6to4 / NAT64 会内嵌 IPv4 地址，构成对 IPv4 检查的直接绕过：
// `64:ff9b::a9fe:a9fe` 在 NAT64 环境会落到 169.254.169.254，
// `2002:0a00:0001::` 会落到 10.0.0.1 —— 即使 AllowPrivate=false 也一样。
var reservedIPv6 = mustCIDRs(
	"fec0::/10",       // 站点本地（已废弃）
	"2001:db8::/32",   // 文档
	"2001:2::/48",     // 基准测试
	"2001::/32",       // Teredo（内嵌 IPv4）
	"2002::/16",       // 6to4（内嵌 IPv4）
	"64:ff9b::/96",    // NAT64（内嵌 IPv4）
	"64:ff9b:1::/48",  // NAT64 本地
	"100::/64",        // 丢弃前缀
	"::/96",           // IPv4-compatible（已废弃，可内嵌 IPv4）
	"::ffff:0:0:0/96", // IPv4-translated（SIIT，可内嵌 IPv4）
)

func mustCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic("netguard: 非法内置 CIDR " + c)
		}
		out = append(out, n)
	}
	return out
}

// CheckIP 返回该 IP 是否允许访问。
func (p Policy) CheckIP(ip net.IP) error {
	if ip == nil {
		return errors.New("目标地址无法解析")
	}
	// ★ 长度必须是 4 或 16。net.IP{} 与截断值（如 net.IP{10,0}）都是非 nil，
	// 但 To4() 返回 nil、Contains 一律 false —— 旧实现会一路 fail open 放行。
	if len(ip) != net.IPv4len && len(ip) != net.IPv6len {
		return fmt.Errorf("目标地址 %s 格式非法", ip)
	}
	// ISATAP 把 IPv4 藏在接口标识里（xxxx:5efe:w.x.y.z），
	// 全局前缀 + ISATAP 不在 reservedIPv6 的网段里，会被当成普通 IPv6 放行。
	if v4 := isatapIPv4(ip); v4 != nil {
		if err := p.CheckIP(v4); err != nil {
			return err
		}
	}
	if ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return fmt.Errorf("目标地址 %s 属于禁止访问的保留网段", ip)
	}
	for _, m := range metadataIPs {
		if ip.Equal(m) {
			return fmt.Errorf("目标地址 %s 是云厂商元数据服务，始终禁止访问", ip)
		}
	}
	// IPv6 保留/过渡网段必须在 AllowPrivate 之前拦截：
	// 6to4 / NAT64 / Teredo / IPv4-compatible 内嵌 IPv4，可绕过上面的 IPv4 检查。
	// 回环地址（::1）本身落在 ::/96 内，放行与否交给下面的私网策略决定。
	if ip.To4() == nil && !ip.IsLoopback() {
		for _, n := range reservedIPv6 {
			if n.Contains(ip) {
				return fmt.Errorf("目标地址 %s 属于不可路由的 IPv6 保留网段", ip)
			}
		}
	}
	if ip4 := ip.To4(); ip4 != nil {
		// 0.0.0.0/8（this network）在部分系统上会被路由到本机
		if ip4[0] == 0 {
			return fmt.Errorf("目标地址 %s 属于禁止访问的保留网段", ip)
		}
		for _, n := range reservedIPv4 {
			if n.Contains(ip4) {
				return fmt.Errorf("目标地址 %s 属于不可路由的保留网段", ip)
			}
		}
	}
	if !p.AllowPrivate {
		// CGNAT 100.64/10 也归入内网（Tailscale 等组网使用该段）
		priv := ip.IsLoopback() || ip.IsPrivate() || (ip.To4() != nil && cgnat.Contains(ip.To4()))
		if priv {
			return fmt.Errorf("目标地址 %s 属于回环/私网，需显式允许内网访问", ip)
		}
	}
	return nil
}

// isatapIPv4 从 ISATAP 地址的接口标识中取出内嵌的 IPv4。
//
// 接口标识形如 0000:5efe:w.x.y.z 或 0200:5efe:w.x.y.z（RFC 5214），
// 即第 9–12 字节为 00 00 5e fe 或 02 00 5e fe。不是 ISATAP 时返回 nil。
func isatapIPv4(ip net.IP) net.IP {
	b := ip.To16()
	if b == nil || ip.To4() != nil {
		return nil
	}
	isatap := (b[8] == 0x00 && b[9] == 0x00 && b[10] == 0x5e && b[11] == 0xfe) ||
		(b[8] == 0x02 && b[9] == 0x00 && b[10] == 0x5e && b[11] == 0xfe)
	if !isatap {
		return nil
	}
	return net.IPv4(b[12], b[13], b[14], b[15]).To4()
}

// CheckHost 对 IP 字面量立即校验；域名返回 nil，实际地址在拨号时校验。
func (p Policy) CheckHost(host string) error {
	if host == "" {
		return errors.New("目标主机为空")
	}
	if ip := net.ParseIP(normalizeIPHost(host)); ip != nil {
		return p.CheckIP(ip)
	}
	return nil
}

// normalizeIPHost 去掉 URL/字面量里的方括号与 IPv6 区域后缀。
// net.ParseIP 两者都不认，原样放行会让 `[::1]`、`fe80::1%eth0`
// 被当成域名，从而绕过地址策略（拨号时再解析到受限地址）。
func normalizeIPHost(host string) string {
	if len(host) >= 2 && host[0] == '[' && host[len(host)-1] == ']' {
		host = host[1 : len(host)-1]
	}
	if i := strings.IndexByte(host, '%'); i >= 0 && strings.Contains(host, ":") {
		host = host[:i]
	}
	return host
}

// CheckURL 校验出站 URL 的协议、主机与地址范围。
func (p Policy) CheckURL(u *url.URL) error {
	if u == nil {
		return errors.New("目标地址为空")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("不支持的协议 %q，仅允许 http/https", u.Scheme)
	}
	if u.Hostname() == "" {
		return errors.New("目标地址缺少主机名")
	}
	return p.CheckHost(u.Hostname())
}

// DialContext 解析并校验目标地址后再建立连接。
func (p Policy) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("目标地址需为 host:port: %w", err)
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	// 超时必须覆盖整个拨号过程（DNS 解析 + 建连），而不是只作用于
	// 单次操作：解析慢或黑洞目标会绕过 net.Dialer.Timeout。
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	d := net.Dialer{Timeout: timeout}

	if ip := net.ParseIP(normalizeIPHost(host)); ip != nil {
		if err := p.CheckIP(ip); err != nil {
			return nil, err
		}
		return d.DialContext(ctx, network, addr)
	}

	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, ipa := range ips {
		if err := p.CheckIP(ipa.IP); err != nil {
			lastErr = err
			continue
		}
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(ipa.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("域名 %s 没有可用地址", host)
	}
	return nil, lastErr
}

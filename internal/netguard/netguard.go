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

// CheckIP 返回该 IP 是否允许访问。
func (p Policy) CheckIP(ip net.IP) error {
	if ip == nil {
		return errors.New("目标地址无法解析")
	}
	if ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return fmt.Errorf("目标地址 %s 属于禁止访问的保留网段", ip)
	}
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 0 {
		// 0.0.0.0/8（this network）在部分系统上会被路由到本机
		return fmt.Errorf("目标地址 %s 属于禁止访问的保留网段", ip)
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

// CheckHost 对 IP 字面量立即校验；域名返回 nil，实际地址在拨号时校验。
func (p Policy) CheckHost(host string) error {
	if host == "" {
		return errors.New("目标主机为空")
	}
	if ip := net.ParseIP(host); ip != nil {
		return p.CheckIP(ip)
	}
	return nil
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
	d := net.Dialer{Timeout: timeout}

	if ip := net.ParseIP(host); ip != nil {
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

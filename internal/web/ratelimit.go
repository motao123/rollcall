package web

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// authFailureLimiter 按来源 IP 限制连续鉴权失败，缓解令牌/密码爆破。
//
// 只对 401 计数；达到阈值后封禁一段时间。成功访问受保护资源会清零计数。
type authFailureLimiter struct {
	mu        sync.Mutex
	state     map[string]*authFailState
	maxFails  int
	window    time.Duration
	block     time.Duration
	lastSweep time.Time
}

type authFailState struct {
	count        int
	windowStart  time.Time
	blockedUntil time.Time
}

func newAuthFailureLimiter() *authFailureLimiter {
	return &authFailureLimiter{
		state:    map[string]*authFailState{},
		maxFails: 10,
		window:   5 * time.Minute,
		block:    10 * time.Minute,
	}
}

// allow 返回是否放行以及剩余封禁时长。
func (l *authFailureLimiter) allow(ip string) (bool, time.Duration) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweepLocked(now)
	st, ok := l.state[ip]
	if !ok {
		return true, 0
	}
	if now.Before(st.blockedUntil) {
		return false, st.blockedUntil.Sub(now)
	}
	if now.Sub(st.windowStart) > l.window {
		delete(l.state, ip)
	}
	return true, 0
}

// sweepLocked 回收过期条目，最多每分钟一次。
//
// 清扫放在 allow 而不是 fail：失败路径由攻击者高频驱动，
// 不能让它承担 O(n) 全表扫描。按时间节流后，高基数下也只是每分钟一次，
// 且正在封禁的条目不会被误删（这正是旧实现「>5000 才扫」的问题）。
func (l *authFailureLimiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < time.Minute {
		return
	}
	l.lastSweep = now
	for k, v := range l.state {
		if now.Sub(v.windowStart) > l.window && now.After(v.blockedUntil) {
			delete(l.state, k)
		}
	}
}

func (l *authFailureLimiter) fail(ip string) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.state[ip]
	if !ok || now.Sub(st.windowStart) > l.window {
		st = &authFailState{windowStart: now}
		l.state[ip] = st
	}
	st.count++
	if st.count >= l.maxFails {
		st.blockedUntil = now.Add(l.block)
		st.count = 0
		st.windowStart = now
	}
	// 过期回收统一交给 allow 中的节流清扫：
	// 不能在攻击者可控的失败路径上做 O(n) 全表扫描。
}

func (l *authFailureLimiter) succeed(ip string) {
	l.mu.Lock()
	delete(l.state, ip)
	l.mu.Unlock()
}

// defaultTrustedProxies 是未显式配置时的唯一可信来源：仅本机回环。
// 反向代理与面板同机时开箱可用；跨机代理必须用 -trusted-proxies 显式声明。
// 不能把「私网」当作可信 —— 同 VPC 内任意主机都能伪造 XFF 绕过限速。
var defaultTrustedProxies = []*net.IPNet{
	{IP: net.IPv4(127, 0, 0, 1), Mask: net.CIDRMask(32, 32)},
	{IP: net.ParseIP("::1"), Mask: net.CIDRMask(128, 128)},
}

// ParseTrustedProxies 解析逗号分隔的 CIDR 列表，也接受单个 IP。
func ParseTrustedProxies(s string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, n, err := net.ParseCIDR(part); err == nil {
			out = append(out, n)
			continue
		}
		ip := net.ParseIP(part)
		if ip == nil {
			return nil, fmt.Errorf("无效的代理地址: %q", part)
		}
		bits := 128
		if v4 := ip.To4(); v4 != nil {
			ip, bits = v4, 32
		}
		out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	return out, nil
}

// clientIP 返回用于限流的客户端地址。
//
// 只有直接从**可信代理**连入时才采信 X-Forwarded-For：从右向左取第一个
// 非可信地址（最后一跳追加的才是真实客户端，左侧可被客户端伪造）。
// 忽略 XFF 会让反代后所有客户端折叠成代理 IP（一次错凭证即全站 429）；
// 无条件信任 XFF 又允许任何人伪造地址绕过限速。
func clientIP(r *http.Request, trusted []*net.IPNet) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !isTrustedProxy(host, trusted) {
		return host
	}
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		cand := strings.TrimSpace(parts[i])
		if cand == "" {
			continue
		}
		if h, _, err := net.SplitHostPort(cand); err == nil {
			cand = h
		}
		if net.ParseIP(cand) == nil || isTrustedProxy(cand, trusted) {
			continue
		}
		return cand
	}
	return host
}

// isTrustedProxy 判断该地址是否在可信代理列表中。
func isTrustedProxy(host string, trusted []*net.IPNet) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// withAuthGuard 在路由外层执行失败限速与审计日志。
// levelOf 由路由表反查，只有受保护级别的 2xx 才会清零失败计数。
func (s *Server) withAuthGuard(h http.Handler, levelOf func(*http.Request) authLevel) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r, s.trustedProxies)
		// 按「来源 IP + 路由鉴权级别」分桶：否则只读面板或 /metrics 的
		// 正常轮询成功后，会把同一 IP 上针对 admin token 的失败计数一并
		// 清零，攻击者每轮试 9 次再等一次正常请求即可无限重试。
		scope := ip + "|" + levelOf(r).String()
		if ok, wait := s.limiter.allow(scope); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			fail(w, http.StatusTooManyRequests, "鉴权失败次数过多，请稍后再试")
			return
		}
		rec := &statusRecorder{ResponseWriter: w}
		h.ServeHTTP(rec, r)
		switch {
		case rec.status == http.StatusUnauthorized:
			s.limiter.fail(scope)
			log.Printf("鉴权失败 ip=%s scope=%s method=%s path=%s", ip, scope, r.Method, r.URL.Path)
		case rec.status > 0 && rec.status < 400 && levelOf(r).protected(s.authUser != ""):
			s.limiter.succeed(scope)
		}
	})
}

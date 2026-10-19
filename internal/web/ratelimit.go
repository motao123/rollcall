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
	fails        int // 已确认的 401 次数
	inflight     int // 已放行但尚未结算的请求
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
// 每次放行都会先占用一个在途名额：否则并发请求会在任何一次 fail()
// 之前全部通过，10 次阈值对并行爆破形同虚设。请求结束后必须调用
// fail（401）、succeed（受保护 2xx）或 release（其余情况）结算。
func (l *authFailureLimiter) allow(ip string) (bool, time.Duration) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweepLocked(now)
	if len(l.state) >= maxLimiterEntries {
		l.evictLocked(now)
	}
	st, ok := l.state[ip]
	if !ok {
		st = &authFailState{windowStart: now}
		l.state[ip] = st
	}
	if now.Before(st.blockedUntil) {
		return false, st.blockedUntil.Sub(now)
	}
	if now.Sub(st.windowStart) > l.window {
		*st = authFailState{windowStart: now}
	}
	// 在途名额给得比失败阈值宽：面板首屏会并发拉多个接口，
	// 若并发数也卡在 maxFails，正常用户会在慢网络下被误伤。
	// 并行爆破最多也只能突破到 2*maxFails 次，仍然有界。
	if st.fails+st.inflight >= l.maxFails*2 {
		return false, 0
	}
	st.inflight++
	return true, 0
}

// maxLimiterEntries 限制来源状态表大小：IPv6 轮换/僵尸网络可以让
// 每条来源都留下一个条目，没有上限时内存会随请求数增长。
const maxLimiterEntries = 10000

// 单次淘汰的工作量上限：allow 在全局锁内调用它，必须保持 O(1) 量级。
// 旧实现每次都在锁内遍历整张 1 万条的表（两遍），高基数来源洪水会把
// 限速器本身变成 CPU 放大器，并串行化所有鉴权请求。
const (
	evictScan  = 256 // 每次最多检查的条目数
	evictBatch = 64  // 每次最多删除的条目数
)

// evictLocked 在状态表打满时回收条目，**有界**且**不误删安全状态**。
//
// 只删除：窗口已过、未在封禁中、没有在途名额的普通条目。
// 封禁中的条目（正在被限速的攻击者）与在途请求必须保留：
// 旧实现会把它们一起删掉，等于在最需要限速的时候把封禁清空。
func (l *authFailureLimiter) evictLocked(now time.Time) {
	freed, scanned := 0, 0

	// 第一遍：优先回收窗口已过且无封禁/无在途的条目
	for k, v := range l.state {
		if freed >= evictBatch || scanned >= evictScan {
			return
		}
		scanned++
		if v.inflight > 0 || now.Before(v.blockedUntil) {
			continue
		}
		if now.Sub(v.windowStart) > l.window {
			delete(l.state, k)
			freed++
		}
	}
	if len(l.state) < maxLimiterEntries {
		return
	}

	// 第二遍：仍然超限时按"最不活跃"腾位，同样跳过封禁/在途条目
	scanned = 0
	for k, v := range l.state {
		if freed >= evictBatch || scanned >= evictScan {
			break
		}
		scanned++
		if v.inflight > 0 || now.Before(v.blockedUntil) {
			continue
		}
		delete(l.state, k)
		freed++
	}
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

// fail 结算一次确定失败。若失败次数达到阈值则封禁一段时间。
func (l *authFailureLimiter) fail(ip string) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.state[ip]
	if !ok {
		st = &authFailState{windowStart: now}
		l.state[ip] = st
	}
	if st.inflight > 0 {
		st.inflight--
	}
	st.fails++
	if st.fails >= l.maxFails {
		st.blockedUntil = now.Add(l.block)
		st.fails = 0
		st.inflight = 0
		st.windowStart = now
	}
	// 过期回收统一交给 allow 中的节流清扫：
	// 不能在攻击者可控的失败路径上做 O(n) 全表扫描。
}

// release 归还未确认失败的在途名额（非 401、非受保护 2xx 的响应）。
func (l *authFailureLimiter) release(ip string) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.state[ip]
	if !ok {
		return
	}
	if st.inflight > 0 {
		st.inflight--
	}
	// 公开页高频轮询不应在状态表里留下常驻条目。
	if st.fails == 0 && st.inflight == 0 && !now.Before(st.blockedUntil) {
		delete(l.state, ip)
	}
}

func (l *authFailureLimiter) succeed(ip string) {
	l.mu.Lock()
	delete(l.state, ip)
	l.mu.Unlock()
}

// resetIP 清除该来源在**所有作用域**下的失败计数与封禁。
//
// ★ 登录成功后必须调用，不能只 succeed(authLogin)：
// 中间件把任何 401 都记为失败，而"会话过期"也会返回 401。
// SPA 首屏会并行拉 2–5 个受保护接口，会话过期时这些 401 全部落在
// IP|readonly / IP|admin 等作用域上；累计到阈值后该 IP 被封 10 分钟。
// 只清 authLogin 的话，用户重新登录成功仍然处处 429，且没有任何自愈路径。
func (l *authFailureLimiter) resetIP(ip string) {
	prefix := rateLimitKey(ip) + "|"
	l.mu.Lock()
	for k := range l.state {
		if strings.HasPrefix(k, prefix) {
			delete(l.state, k)
		}
	}
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
	// 有的代理（如 HAProxy）会把真实客户端追加成新的一行，
	// Header.Get 只拿第一行，必须把所有行按序拼起来再从右向左找。
	xff := strings.Join(r.Header.Values("X-Forwarded-For"), ",")
	parts := strings.Split(xff, ",")
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

// rateLimitKey 把 IPv6 地址折叠到 /64 再分桶：一个终端通常握有整段 /64，
// 逐地址分桶等于给攻击者每请求一次免费换桶的机会，阈值永远触发不了。
func rateLimitKey(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() != nil {
		return ip
	}
	return parsed.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	// 1xx 是中间响应，后面还会有最终状态码，不能记为最终结果。
	if code >= 100 && code < 200 {
		r.ResponseWriter.WriteHeader(code)
		return
	}
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

// Unwrap 让 http.ResponseController 能拿到原始 ResponseWriter。
// 没有它时，SetReadDeadline / Flush / Hijack 这类能力在包了限速中间件后
// 全部失效（Go 1.20+ 的标准探测方式就是 Unwrap）。
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// withAuthGuard 在路由外层执行失败限速与审计日志。
// levelOf 由路由表反查，只有受保护级别的 2xx 才会清零失败计数。
func (s *Server) withAuthGuard(h http.Handler, levelOf func(*http.Request) authLevel) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r, s.trustedProxies)
		level := levelOf(r)
		// 按「来源 IP + 路由鉴权级别」分桶：否则只读面板或 /metrics 的
		// 正常轮询成功后，会把同一 IP 上针对 admin token 的失败计数一并
		// 清零，攻击者每轮试 9 次再等一次正常请求即可无限重试。
		scope := rateLimitKey(ip) + "|" + level.String()
		if ok, wait := s.limiter.allow(scope); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			fail(w, http.StatusTooManyRequests, "鉴权失败次数过多，请稍后再试")
			return
		}
		rec := &statusRecorder{ResponseWriter: w}
		defer func() {
			// handler panic 时也要归还名额，否则每次崩溃都会永久
			// 消耗一个在途名额，十次之后整个来源被误封。
			if p := recover(); p != nil {
				s.limiter.release(scope)
				panic(p)
			}
		}()
		h.ServeHTTP(rec, r)
		// 处理器什么都没写时由 net/http 隐式返回 200，此时 recorder
		// 里仍是零值；补成 200，受保护路由的成功请求才能正常清零计数。
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		switch {
		case rec.status == http.StatusUnauthorized:
			s.limiter.fail(scope)
			log.Printf("鉴权失败 ip=%s level=%s method=%s path=%q", ip, level, r.Method, r.URL.Path)
		case rec.status >= 200 && rec.status < 300 && level.protected():
			// 只认 2xx：3xx 不代表鉴权通过（例如登录跳转），
			// 用它清零会让攻击者在拿到重定向后继续重试。
			s.limiter.succeed(scope)
		default:
			// 公开页 2xx、404/400、3xx 等都归还名额，只是不清零失败计数。
			s.limiter.release(scope)
		}
	})
}

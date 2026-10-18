package web

import (
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// authFailureLimiter 按来源 IP 限制连续鉴权失败，缓解令牌/密码爆破。
//
// 只对 401 计数；达到阈值后封禁一段时间。成功访问受保护资源会清零计数。
type authFailureLimiter struct {
	mu       sync.Mutex
	state    map[string]*authFailState
	maxFails int
	window   time.Duration
	block    time.Duration
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
		// 封禁到期后由 allow 清理，避免 map 无限增长
	}
	if len(l.state) > 5000 {
		for k, v := range l.state {
			if now.Sub(v.windowStart) > l.window && now.After(v.blockedUntil) {
				delete(l.state, k)
			}
		}
	}
}

func (l *authFailureLimiter) succeed(ip string) {
	l.mu.Lock()
	delete(l.state, ip)
	l.mu.Unlock()
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
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
		ip := clientIP(r)
		if ok, wait := s.limiter.allow(ip); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			fail(w, http.StatusTooManyRequests, "鉴权失败次数过多，请稍后再试")
			return
		}
		rec := &statusRecorder{ResponseWriter: w}
		h.ServeHTTP(rec, r)
		switch {
		case rec.status == http.StatusUnauthorized:
			s.limiter.fail(ip)
			log.Printf("鉴权失败 ip=%s method=%s path=%s", ip, r.Method, r.URL.Path)
		case rec.status > 0 && rec.status < 400 && levelOf(r).protected(s.authUser != ""):
			s.limiter.succeed(ip)
		}
	})
}

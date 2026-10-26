package web

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"sync"
	"time"
)

// publicStatusCookie 是公开页通过 ?t=<token> 首次访问后种下的凭据，
// 让页面里的 status.js、样式与 SSE 不必在每个请求上重复带查询串。
const publicStatusCookie = "rollcall_st"

// publicGate 给公开页及其全部资源套上"总开关 + 访问令牌"。
//
// 两层门槛的意义不同：
//   - Enabled 是总开关。v1.25.0 起默认开启（产品决策：探针装好就该有
//     状态页，脱敏保证不变）；显式关闭后对匿名一律 404。
//   - Token 是"知道链接才进得来"，不是账号体系；为空表示不校验。
//
// 关闭时一律返回 404 而不是 403：403 等于告诉扫描器"这里确实有个页面"。
func (s *Server) publicGate(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.st.StatusPage()
		if !cfg.IsEnabled() {
			publicNotFound(w, r)
			return
		}
		if want := strings.TrimSpace(cfg.Token); want != "" && !s.publicTokenOK(w, r, want) {
			publicNotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// publicNotFound 对 API 路径回 JSON，对页面与静态资源回纯文本 404。
// 前端拿到 HTML 404 会解析失败并弹出无意义的报错，这里保持形状一致。
func publicNotFound(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		fail(w, http.StatusNotFound, "not found")
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

// tokenFailLimiter 限制公开页访问令牌的在线枚举。
//
// 令牌是「知道链接才进得来」模型，常量时间比较只能防侧信道，防不了
// 高速在线枚举：不设限速时，攻击者可以对 ?t= 每秒成百次地猜。
// 必须用**独立的失败桶**而不是复用鉴权限速——这里的合法访客是匿名的，
// 与登录/鉴权的 401 计数互不干扰，也不会被对方清零或挤占。
type tokenFailLimiter struct {
	mu    sync.Mutex
	state map[string]*tokenFailState
}

const (
	// tokenFailWindow 是失败计数的固定窗口；窗口内超限后封禁到窗口结束。
	tokenFailWindow = time.Minute
	// tokenFailMax 是窗口内允许的失败次数。正常访客输错链接不会超过它；
	// 30 次对人工足够宽容，对枚举器是 30 次/分钟 = 每天 4.3 万次，
	// 面对 ≥16 位随机令牌（62^16）完全不可行。
	tokenFailMax = 30
	// tokenFailCap 是状态表条目上限，防伪造 IP 打爆内存（真实来源 IP
	// 不可伪造，这里只防经过可信代理时的伪造 XFF）。
	tokenFailCap = 8192
)

type tokenFailState struct {
	fails     int
	windowEnd int64
	blockedTo int64
}

func newTokenFailLimiter() *tokenFailLimiter {
	return &tokenFailLimiter{state: make(map[string]*tokenFailState)}
}

// allow 报告该来源此刻能否尝试一次令牌校验。
func (l *tokenFailLimiter) allow(ip string, now int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.state[ip]
	if !ok {
		return true
	}
	if now < st.blockedTo {
		return false
	}
	if now >= st.windowEnd {
		delete(l.state, ip)
	}
	return true
}

// fail 记录一次失败；返回该来源是否已被封禁。
func (l *tokenFailLimiter) fail(ip string, now int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.state[ip]
	if !ok || now >= st.windowEnd {
		if len(l.state) >= tokenFailCap {
			// 清理过期条目；仍满则整体清空（与 nonce 表同一取舍：
			// 丢计数好过让合法访客被无差别拒绝）。
			for k, v := range l.state {
				if now >= v.windowEnd && now >= v.blockedTo {
					delete(l.state, k)
				}
			}
			if len(l.state) >= tokenFailCap {
				l.state = make(map[string]*tokenFailState)
			}
		}
		st = &tokenFailState{windowEnd: now + int64(tokenFailWindow/time.Second)}
		l.state[ip] = st
	}
	st.fails++
	if st.fails >= tokenFailMax {
		st.blockedTo = st.windowEnd
		return true
	}
	return false
}

// succeed 清除该来源的失败计数（令牌校验成功时调用）。
func (l *tokenFailLimiter) succeed(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.state, ip)
}

// publicTokenOK 校验访问令牌，命中查询串时顺手种 Cookie。
//
// 失败尝试走 tokenFailLimiter 独立计数（在线枚举防护，v1.35.2）；
// 旧注释"不做限速"的理由已不成立——失败桶只统计**校验失败**，
// 携带正确令牌的正常轮询不经过任何计数。
func (s *Server) publicTokenOK(w http.ResponseWriter, r *http.Request, want string) bool {
	ip := clientIP(r, s.trustedProxies)
	now := time.Now().Unix()
	if !s.tokenFails.allow(ip, now) {
		return false
	}
	match := func(got string) bool {
		ok := subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), []byte(want)) == 1
		if ok {
			s.tokenFails.succeed(ip)
		} else {
			s.tokenFails.fail(ip, now)
		}
		return ok
	}

	if got := r.URL.Query().Get("t"); got != "" {
		if !match(got) {
			return false
		}
		// 只有真的对上了才种 Cookie，避免把错误 token 写进浏览器。
		// Secure 走 requestIsHTTPS（可信代理的 X-Forwarded-Proto）而不是
		// r.TLS：TLS 由反代终止时 r.TLS 恒为 nil，用它会漏掉 Secure 标志
		//（与会话 Cookie 的判定保持一致，v1.35.2）。
		http.SetCookie(w, &http.Cookie{
			Name:     publicStatusCookie,
			Value:    want,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Secure:   s.requestIsHTTPS(r),
			MaxAge:   int((30 * 24 * time.Hour).Seconds()),
		})
		return true
	}
	if got := r.Header.Get("X-Status-Token"); got != "" {
		return match(got)
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return match(strings.TrimPrefix(auth, "Bearer "))
	}
	if c, err := r.Cookie(publicStatusCookie); err == nil {
		return match(c.Value)
	}
	return false
}

// statusGateEnabled 供测试与内部调用判断公开页是否开放。
func (s *Server) statusGateEnabled() bool { return s.st.StatusPage().IsEnabled() }

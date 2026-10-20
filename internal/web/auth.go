package web

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/motao/rollcall/internal/auth"
)

const sessionCookieName = "rollcall_session"

// revokedSessionsKey 是持久化的会话吊销列表（令牌指纹 -> 过期 Unix 秒）。
const revokedSessionsKey = "revoked_sessions"

type loginRequest struct {
	Identifier string `json:"identifier"`
	Password   string `json:"password"`
	TOTP       string `json:"totp"`
}

// handleAuthSession 返回当前登录态。未登录时返回 200，避免把正常的
// 首屏探测计入登录失败限速。
func (s *Server) handleAuthSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if sess, u, ok := s.sessionUserWithUser(r); ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated": true,
			"user":          u.PublicInfo(),
			"expiresAt":     sess.Expires.Unix(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
}

// handleAuthLogin 校验用户名/邮箱 + 密码并写入 HttpOnly 会话 Cookie。
func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	u := s.currentUser()
	if u == nil || s.sessions == nil {
		fail(w, http.StatusServiceUnavailable, "面板登录未启用")
		return
	}
	// 只接受 JSON：跨站 HTML 表单只能发 text/plain / form-urlencoded，
	// 不校验就会形成 login CSRF（把受害者登进攻击者的账号）。
	if ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type"))); !strings.HasPrefix(ct, "application/json") {
		fail(w, http.StatusUnsupportedMediaType, "Content-Type 必须为 application/json")
		return
	}
	body := http.MaxBytesReader(w, r.Body, 4<<10)
	var in loginRequest
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		fail(w, http.StatusBadRequest, "请求包含多余内容")
		return
	}
	in.Identifier = strings.TrimSpace(in.Identifier)
	if in.Identifier == "" || len(in.Identifier) > 254 || in.Password == "" || len(in.Password) > 1024 {
		fail(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	if !u.Authenticate(in.Identifier, in.Password) {
		fail(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	// 密码通过后再要求动态码：一次性把「缺少动态码」返回给前端，
	// 由前端切换到验证码输入步骤；不存在的账号不会走到这里。
	if u.TOTPEnabled {
		if strings.TrimSpace(in.TOTP) == "" {
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": "请输入动态验证码", "totpRequired": true,
			})
			return
		}
		// ★ 原子消费时间步：同一动态码只能成功登录一次。
		// 只用 VerifyTOTP 校验不落盘已用步进，码在 ±1 窗口（约 90 秒）内可重放；
		// 与账号修改共用 accountMu，保证并发登录也只有一个能消费成功。
		s.accountMu.Lock()
		cur := s.currentUser()
		if cur == nil {
			s.accountMu.Unlock()
			fail(w, http.StatusServiceUnavailable, "面板登录未启用")
			return
		}
		updated, err := s.consumeTOTPLocked(cur, in.TOTP)
		s.accountMu.Unlock()
		if err != nil {
			switch {
			case errors.Is(err, errTOTPRequired):
				w.Header().Set("Cache-Control", "no-store")
				writeJSON(w, http.StatusUnauthorized, map[string]any{
					"error": err.Error(), "totpRequired": true,
				})
			case errors.Is(err, auth.ErrTOTPReplayed):
				fail(w, http.StatusUnauthorized, err.Error())
			default:
				fail(w, http.StatusUnauthorized, "动态验证码错误")
			}
			return
		}
		u = updated
	}

	token, exp, err := s.sessions.Issue(u.Username, u.SessionVersion())
	if err != nil {
		log.Printf("签发面板会话失败: %v", err)
		fail(w, http.StatusInternalServerError, "登录失败，请稍后再试")
		return
	}
	s.setSessionCookie(w, r, token, exp)
	ip := clientIP(r, s.trustedProxies)
	// 登录端点的 2xx 不参与通用清零（否则探测 /api/auth/session 就能
	// 重置爆破计数），成功登录在这里显式清零。
	//
	// ★ 用 resetIP 而不是只清 authLogin：会话过期产生的 401 计在
	// IP|readonly / IP|admin 等作用域上，只清 authLogin 会让"重新登录"
	// 无法解除那些封禁，用户登录成功却满屏 429（10 分钟内不可自愈）。
	s.limiter.resetIP(ip)
	s.clearInitialPassword()
	log.Printf("面板登录成功 user=%s ip=%s", u.Username, ip)
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"user":          u.PublicInfo(),
		"expiresAt":     exp.Unix(),
	})
}

// clearInitialPassword 删除首启落盘的随机密码。
// 成功登录后该密码已被使用（或已被用户改掉），继续留在数据目录只会
// 被备份、快照或误读的日志带走。删除失败不影响登录。
func (s *Server) clearInitialPassword() {
	if s.userPath == "" {
		return
	}
	p := filepath.Join(filepath.Dir(s.userPath), auth.InitialPasswordFile)
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("删除初始密码文件失败: %v", err)
	}
}

// handleAuthLogout 清除会话 Cookie，并吊销当前令牌。重复调用是幂等的。
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" && s.sessions != nil {
		// 只有验签通过的令牌才值得写入吊销表，避免未认证请求把表撑大。
		if sess, ok := s.sessions.Verify(c.Value); ok {
			s.revokeSession(c.Value, sess.Expires)
		}
	}
	s.clearSessionCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// loadRevokedSessions 从设置里恢复吊销列表，并丢弃已过期条目。
func (s *Server) loadRevokedSessions() {
	s.revoked = map[string]int64{}
	raw := s.st.GetSetting(revokedSessionsKey)
	if raw == "" {
		return
	}
	var m map[string]int64
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		log.Printf("会话吊销列表损坏，已忽略: %v", err)
		return
	}
	now := time.Now().Unix()
	for id, exp := range m {
		if exp > now {
			s.revoked[id] = exp
		}
	}
}

// revokeSession 把令牌指纹写入吊销表并立即落盘：
// 只走防抖保存时，登出后立刻重启会让被吊销的令牌重新可用。
func (s *Server) revokeSession(token string, exp time.Time) {
	if s.sessions == nil || token == "" {
		return
	}
	id := s.sessions.TokenID(token)
	now := time.Now().Unix()
	s.revokedMu.Lock()
	if s.revoked == nil {
		s.revoked = map[string]int64{}
	}
	for k, v := range s.revoked {
		if v <= now {
			delete(s.revoked, k)
		}
	}
	s.revoked[id] = exp.Unix()
	b, err := json.Marshal(s.revoked)
	s.revokedMu.Unlock()
	if err != nil {
		log.Printf("序列化会话吊销列表失败: %v", err)
		return
	}
	s.st.SetSetting(revokedSessionsKey, string(b))
	if err := s.st.Save(); err != nil {
		log.Printf("保存会话吊销列表失败: %v", err)
	}
}

func (s *Server) sessionRevoked(token string) bool {
	if s.sessions == nil || token == "" {
		return false
	}
	id := s.sessions.TokenID(token)
	now := time.Now().Unix()
	s.revokedMu.Lock()
	exp, ok := s.revoked[id]
	if ok && exp <= now {
		delete(s.revoked, id)
		ok = false
	}
	s.revokedMu.Unlock()
	return ok
}

func (s *Server) sessionUser(r *http.Request) (sess authSession, ok bool) {
	sess, _, ok = s.sessionUserWithUser(r)
	return sess, ok
}

// sessionUserWithUser 同时返回校验通过的账号快照，
// 避免调用方再取一次指针时读到刚被替换的新账号。
func (s *Server) sessionUserWithUser(r *http.Request) (sess authSession, u *auth.User, ok bool) {
	u = s.currentUser()
	if u == nil || s.sessions == nil {
		return authSession{}, nil, false
	}
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return authSession{}, nil, false
	}
	got, ok := s.sessions.Verify(c.Value)
	if !ok {
		return authSession{}, nil, false
	}
	if !strings.EqualFold(got.Username, u.Username) || got.Version != u.SessionVersion() {
		return authSession{}, nil, false
	}
	if s.sessionRevoked(c.Value) {
		return authSession{}, nil, false
	}
	return authSession{Username: got.Username, Expires: got.Expires}, u, true
}

func (s *Server) sessionOK(r *http.Request) bool {
	_, ok := s.sessionUser(r)
	return ok
}

type authSession struct {
	Username string
	Expires  time.Time
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, exp time.Time) {
	maxAge := int(time.Until(exp).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.requestIsHTTPS(r),
		SameSite: http.SameSiteStrictMode,
		Expires:  exp,
		MaxAge:   maxAge,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.requestIsHTTPS(r),
		SameSite: http.SameSiteStrictMode,
		Expires:  time.Unix(1, 0),
		MaxAge:   -1,
	})
}

func (s *Server) requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	// X-Forwarded-Proto 可被任意客户端伪造；只有请求确实来自可信代理时
	// 才采信。否则直连 HTTP 的客户端可以声称 https，让 Cookie 带上
	// Secure 后被浏览器丢弃（或反过来误导安全判断）。
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !isTrustedProxy(host, s.trustedProxies) {
		return false
	}
	parts := strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		if v := strings.TrimSpace(parts[i]); v != "" {
			return strings.EqualFold(v, "https")
		}
	}
	return false
}

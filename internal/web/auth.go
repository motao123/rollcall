package web

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const sessionCookieName = "rollcall_session"

type loginRequest struct {
	Identifier string `json:"identifier"`
	Password   string `json:"password"`
}

// handleAuthSession 返回当前登录态。未登录时返回 200，避免把正常的
// 首屏探测计入登录失败限速。
func (s *Server) handleAuthSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if sess, ok := s.sessionUser(r); ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated": true,
			"user":          s.user.PublicInfo(),
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
	if s.user == nil || s.sessions == nil {
		fail(w, http.StatusServiceUnavailable, "面板登录未启用")
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
	if !s.user.Authenticate(in.Identifier, in.Password) {
		fail(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}

	token, exp, err := s.sessions.Issue(s.user.Username, s.user.SessionVersion())
	if err != nil {
		log.Printf("签发面板会话失败: %v", err)
		fail(w, http.StatusInternalServerError, "登录失败，请稍后再试")
		return
	}
	s.setSessionCookie(w, r, token, exp)
	ip := clientIP(r, s.trustedProxies)
	// 登录端点的 2xx 不参与通用清零（否则探测 /api/auth/session 就能
	// 重置爆破计数），成功登录在这里显式清零。
	s.limiter.succeed(ip + "|" + authLogin.String())
	log.Printf("面板登录成功 user=%s ip=%s", s.user.Username, ip)
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"user":          s.user.PublicInfo(),
		"expiresAt":     exp.Unix(),
	})
}

// handleAuthLogout 清除会话 Cookie。重复调用是幂等的。
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.clearSessionCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) sessionUser(r *http.Request) (sess authSession, ok bool) {
	if s.user == nil || s.sessions == nil {
		return authSession{}, false
	}
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return authSession{}, false
	}
	got, ok := s.sessions.Verify(c.Value)
	if !ok {
		return authSession{}, false
	}
	if !strings.EqualFold(got.Username, s.user.Username) || got.Version != s.user.SessionVersion() {
		return authSession{}, false
	}
	return authSession{Username: got.Username, Expires: got.Expires}, true
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
		Secure:   requestIsHTTPS(r),
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
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteStrictMode,
		Expires:  time.Unix(1, 0),
		MaxAge:   -1,
	})
}

func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	for _, v := range strings.Split(r.Header.Get("X-Forwarded-Proto"), ",") {
		if strings.EqualFold(strings.TrimSpace(v), "https") {
			return true
		}
	}
	return false
}

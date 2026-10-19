package web

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/motao/rollcall/internal/auth"
)

type accountProfileRequest struct {
	Username        string `json:"username"`
	Email           string `json:"email"`
	CurrentPassword string `json:"currentPassword"`
}

type accountPasswordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

// currentUser 返回账号快照。User 一经发布不再原地修改，
// 因此调用方拿到指针后无需继续持锁。
func (s *Server) currentUser() *auth.User {
	s.userMu.RLock()
	defer s.userMu.RUnlock()
	return s.user
}

func (s *Server) setUser(u *auth.User) {
	s.userMu.Lock()
	s.user = u
	s.userMu.Unlock()
}

// handleAdminAccount 修改用户名/邮箱，要求确认当前密码。
func (s *Server) handleAdminAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.accountChange(w, r, func(w http.ResponseWriter, r *http.Request, current *auth.User) {
		var in accountProfileRequest
		if !decodeAccountJSON(w, r, &in) {
			return
		}
		username := strings.TrimSpace(in.Username)
		email := strings.TrimSpace(in.Email)
		if err := auth.ValidateUsername(username); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := auth.ValidateEmail(email); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		if in.CurrentPassword == "" {
			fail(w, http.StatusBadRequest, "请输入当前密码")
			return
		}
		viaSession := s.sessionOK(r)
		if !auth.VerifyPassword(current.PasswordHash, in.CurrentPassword) {
			fail(w, http.StatusForbidden, "当前密码不正确")
			return
		}
		updated, err := auth.UpdateProfile(s.userPath, current, username, email)
		if err != nil {
			log.Printf("保存管理员账号失败: %v", err)
			fail(w, http.StatusInternalServerError, "保存账号失败: "+err.Error())
			return
		}
		s.setUser(updated)
		log.Printf("管理员账号已更新 user=%s ip=%s", updated.Username, clientIP(r, s.trustedProxies))
		s.accountResponse(w, r, updated, viaSession)
	})
}

// handleAdminAccountPassword 修改密码，要求确认当前密码。
// 成功后版本号递增，所有旧会话失效；当前浏览器换发新 Cookie。
func (s *Server) handleAdminAccountPassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.accountChange(w, r, func(w http.ResponseWriter, r *http.Request, current *auth.User) {
		var in accountPasswordRequest
		if !decodeAccountJSON(w, r, &in) {
			return
		}
		if in.CurrentPassword == "" {
			fail(w, http.StatusBadRequest, "请输入当前密码")
			return
		}
		if err := auth.ValidatePassword(in.NewPassword); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		viaSession := s.sessionOK(r)
		updated, err := auth.UpdatePassword(s.userPath, current, in.CurrentPassword, in.NewPassword)
		switch {
		case errors.Is(err, auth.ErrPasswordMismatch):
			fail(w, http.StatusForbidden, err.Error())
			return
		case err != nil:
			log.Printf("保存管理员密码失败: %v", err)
			fail(w, http.StatusInternalServerError, "保存密码失败: "+err.Error())
			return
		}
		s.setUser(updated)
		log.Printf("管理员密码已修改 user=%s ip=%s", updated.Username, clientIP(r, s.trustedProxies))
		s.accountResponse(w, r, updated, viaSession)
	})
}

// accountChange 串行化账号修改并单独限制「当前密码」爆破。
//
// 路由层的失败限速只统计 401；这里密码错误返回 403（避免前端把
// 会话判失效），必须显式记账，否则被窃取的会话可无限猜当前密码。
func (s *Server) accountChange(w http.ResponseWriter, r *http.Request, change func(http.ResponseWriter, *http.Request, *auth.User)) {
	if s.currentUser() == nil || s.sessions == nil {
		fail(w, http.StatusServiceUnavailable, "面板登录未启用")
		return
	}
	if s.userPath == "" {
		fail(w, http.StatusServiceUnavailable, "在线修改管理员账号未启用")
		return
	}
	scope := rateLimitKey(clientIP(r, s.trustedProxies)) + "|account"
	if ok, wait := s.limiter.allow(scope); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		fail(w, http.StatusTooManyRequests, "尝试次数过多，请稍后再试")
		return
	}
	rec := &statusRecorder{ResponseWriter: w}
	func() {
		s.accountMu.Lock()
		defer s.accountMu.Unlock()
		current := s.currentUser()
		if current == nil {
			fail(rec, http.StatusServiceUnavailable, "面板登录未启用")
			return
		}
		change(rec, r, current)
	}()
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	switch {
	case rec.status == http.StatusForbidden:
		s.limiter.fail(scope)
	case rec.status >= 200 && rec.status < 300:
		s.limiter.succeed(scope)
	default:
		s.limiter.release(scope)
	}
}

// accountResponse 下发更新后的账号信息；浏览器会话需换发令牌，
// 否则版本号递增会让当前标签页在下一个请求就被登出。
func (s *Server) accountResponse(w http.ResponseWriter, r *http.Request, u *auth.User, reissue bool) {
	resp := map[string]any{"user": u.PublicInfo()}
	if reissue {
		token, exp, err := s.sessions.Issue(u.Username, u.SessionVersion())
		if err != nil {
			log.Printf("更新账号后签发会话失败: %v", err)
			fail(w, http.StatusInternalServerError, "账号已保存，但会话续签失败，请重新登录")
			return
		}
		s.setSessionCookie(w, r, token, exp)
		resp["authenticated"] = true
		resp["expiresAt"] = exp.Unix()
	}
	writeJSON(w, http.StatusOK, resp)
}

func decodeAccountJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	body := http.MaxBytesReader(w, r.Body, 8<<10)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		fail(w, http.StatusBadRequest, "bad json: "+err.Error())
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		fail(w, http.StatusBadRequest, "请求包含多余内容")
		return false
	}
	return true
}

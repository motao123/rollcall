package web

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/motao/rollcall/internal/auth"
)

type accountProfileRequest struct {
	Username        string `json:"username"`
	Email           string `json:"email"`
	CurrentPassword string `json:"currentPassword"`
	// Code 是启用两步验证后必须提供的动态码；未启用时忽略。
	Code string `json:"code"`
}

type accountPasswordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
	// Code 是启用两步验证后必须提供的动态码；未启用时忽略。
	Code string `json:"code"`
}

// TOTP 校验的两类失败：没填 vs 填错/重放。
// 前者不应计入限速（用户只是还没填），后者必须计入。
var (
	errTOTPRequired = errors.New("请输入动态验证码")
	errTOTPInvalid  = errors.New("动态验证码错误")
)

// consumeTOTPLocked 校验并消费一次性动态码，返回更新后的用户。
//
// ★ 必须在持有 s.accountMu 时调用：登录与账号修改共用这把锁，
// 否则两个并发请求可以用同一个动态码各拿到一次授权（重放）。
// 账号未启用 TOTP 时直接返回原用户（无操作）。
func (s *Server) consumeTOTPLocked(current *auth.User, code string) (*auth.User, error) {
	if current == nil || !current.TOTPEnabled {
		return current, nil
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, errTOTPRequired
	}
	step, ok := auth.VerifyTOTPStep(current.TOTPSecret, code, time.Now())
	if !ok {
		return nil, errTOTPInvalid
	}
	updated, err := auth.MarkTOTPStep(s.userPath, current, step)
	if err != nil {
		return nil, err
	}
	s.setUser(updated)
	return updated, nil
}

// failTOTP 把 consume 的错误映射为响应；已计入限速的状态码用 403。
func failTOTP(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errTOTPRequired):
		fail(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, auth.ErrTOTPReplayed):
		// 403 会被 accountChange 计入失败：重放尝试属于攻击行为。
		fail(w, http.StatusForbidden, err.Error())
	default:
		fail(w, http.StatusForbidden, "动态验证码错误")
	}
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
		// 启用两步验证后，改用户名/邮箱也必须过第二因子：
		// 只验密码的话，被窃会话 + 密码就能完成账号接管。
		cur, err := s.consumeTOTPLocked(current, in.Code)
		if err != nil {
			failTOTP(w, err)
			return
		}
		updated, err := auth.UpdateProfile(s.userPath, cur, username, email)
		if err != nil {
			log.Printf("保存管理员账号失败: %v", err)
			// 不把内部错误回显给客户端：saveUser 的错误是 *os.PathError，
			// 会把 admin-user.json 的绝对路径与文件系统细节泄露出去。
			fail(w, http.StatusInternalServerError, "保存账号失败")
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
		// 改密码是最关键的账号接管路径，启用 2FA 后必须验证动态码。
		cur, err := s.consumeTOTPLocked(current, in.Code)
		if err != nil {
			failTOTP(w, err)
			return
		}
		updated, err := auth.UpdatePassword(s.userPath, cur, in.CurrentPassword, in.NewPassword)
		switch {
		case errors.Is(err, auth.ErrPasswordMismatch):
			fail(w, http.StatusForbidden, err.Error())
			return
		case err != nil:
			log.Printf("保存管理员密码失败: %v", err)
			fail(w, http.StatusInternalServerError, "保存密码失败")
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

type accountTOTPRequest struct {
	Action          string `json:"action"` // setup | enable | disable
	CurrentPassword string `json:"currentPassword"`
	Code            string `json:"code"`
}

// handleAdminAccountTOTP 管理两步验证：生成密钥、启用、停用。
func (s *Server) handleAdminAccountTOTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.accountChange(w, r, func(w http.ResponseWriter, r *http.Request, current *auth.User) {
		var in accountTOTPRequest
		if !decodeAccountJSON(w, r, &in) {
			return
		}
		if in.CurrentPassword == "" || !auth.VerifyPassword(current.PasswordHash, in.CurrentPassword) {
			fail(w, http.StatusForbidden, "当前密码不正确")
			return
		}
		// ★ 必须在启用/停用（会递增会话版本）之前判断来源：
		// 版本一涨，请求里携带的旧 Cookie 立刻失配，事后再调用 sessionOK
		// 会得到 false，于是浏览器用户虽然成功启用 2FA 却被登出。
		viaSession := s.sessionOK(r)
		switch in.Action {
		case "setup":
			if current.TOTPEnabled {
				fail(w, http.StatusBadRequest, "两步验证已启用，请先停用再重新绑定")
				return
			}
			secret, err := auth.NewTOTPSecret()
			if err != nil {
				log.Printf("生成 TOTP 密钥失败: %v", err)
				fail(w, http.StatusInternalServerError, "生成 TOTP 密钥失败")
				return
			}
			updated, err := auth.UpdateTOTP(s.userPath, current, secret, false)
			if err != nil {
				log.Printf("保存 TOTP 密钥失败: %v", err)
				fail(w, http.StatusInternalServerError, "保存 TOTP 密钥失败")
				return
			}
			s.setUser(updated)
			writeJSON(w, http.StatusOK, map[string]any{
				"enabled": false,
				"secret":  secret,
				"otpauth": auth.TOTPURL("Rollcall", updated.Username, secret),
			})
		case "enable":
			if strings.TrimSpace(in.Code) == "" {
				fail(w, http.StatusBadRequest, "请输入身份验证器中的 6 位动态码")
				return
			}
			if current.TOTPSecret == "" {
				fail(w, http.StatusBadRequest, "请先生成 TOTP 密钥")
				return
			}
			if !auth.VerifyTOTP(current.TOTPSecret, in.Code, time.Now()) {
				// ★ 用 403 而不是 400：accountChange 只对 403 记失败。
				// 用 400 时动态码错误落在 default: release 分支，计数永不增长，
				// 拿到会话+当前密码即可无限穷举 6 位码把 2FA 关掉。
				fail(w, http.StatusForbidden, "动态验证码错误，请确认手机时间准确")
				return
			}
			updated, err := auth.UpdateTOTP(s.userPath, current, current.TOTPSecret, true)
			if err != nil {
				log.Printf("启用 TOTP 失败: %v", err)
				fail(w, http.StatusInternalServerError, "启用 TOTP 失败")
				return
			}
			s.setUser(updated)
			log.Printf("管理员已启用 TOTP user=%s ip=%s", updated.Username, clientIP(r, s.trustedProxies))
			// 只有浏览器会话需要换发 Cookie；用 API 令牌调用时不该收到 Set-Cookie。
			s.accountResponse(w, r, updated, viaSession)
		case "disable":
			// 停用同样消费一次性动态码：否则同一个码可以被反复用来试停用。
			cur, err := s.consumeTOTPLocked(current, in.Code)
			if err != nil {
				failTOTP(w, err)
				return
			}
			updated, err := auth.UpdateTOTP(s.userPath, cur, "", false)
			if err != nil {
				log.Printf("停用 TOTP 失败: %v", err)
				fail(w, http.StatusInternalServerError, "停用 TOTP 失败")
				return
			}
			s.setUser(updated)
			log.Printf("管理员已停用 TOTP user=%s ip=%s", updated.Username, clientIP(r, s.trustedProxies))
			s.accountResponse(w, r, updated, viaSession)
		default:
			fail(w, http.StatusBadRequest, "action 必须是 setup / enable / disable")
		}
	})
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

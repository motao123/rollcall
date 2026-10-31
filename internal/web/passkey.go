package web

// Passkey 的四段 ceremony 与凭据管理。
//
// 与密码登录的关系：
//   - Passkey 是独立的完整登录方式（设备持有 + 生物识别/PIN = 多因素），
//     登录成功直接铸会话，不再叠加 TOTP
//   - 密码 + TOTP 的登录不受影响；未注册 Passkey 时登录层按钮返回明确错误
//
// 挑战（challenge）的防重放：
//   - begin 端点生成 32 字节随机挑战，服务端以随机 ID 键存内存（单次消费、5 分钟过期）
//   - finish 端点按 ID 取出并立即删除：同一挑战绝不能通过两次验证
//   - 挑战上限 512 条：防未认证请求把内存灌满（authLogin 级别自带 IP 限速）

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/motao123/rollcall/internal/auth"
	"github.com/motao123/rollcall/internal/webauthn"
)

const (
	passkeyChallengeTTL  = 5 * time.Minute
	passkeyChallengeMax  = 512
	passkeyTimeoutMS     = 120000
	passkeyMaxPerAccount = 20
)

type passkeyChallenge struct {
	value []byte
	exp   time.Time
	// rp 记录发起时的依赖方：finish 时重新推导并比对，防止
	// begin（回环 http）与 finish（域名 https）跨来源混用。
	rp      webauthn.RelyingParty
	purpose string // "register" | "login"
}

type passkeyChallengeStore struct {
	mu         sync.Mutex
	challenges map[string]passkeyChallenge
}

func (s *Server) newPasskeyChallenge(purpose string, rp webauthn.RelyingParty) (string, []byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, err
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return "", nil, err
	}
	chID := base64.RawURLEncoding.EncodeToString(id)
	now := time.Now()

	s.passkeyMu.Lock()
	defer s.passkeyMu.Unlock()
	if s.passkeyChallenges == nil {
		s.passkeyChallenges = map[string]passkeyChallenge{}
	}
	// 顺带清理过期挑战，防止 map 随时间无限增长
	for k, c := range s.passkeyChallenges {
		if now.After(c.exp) {
			delete(s.passkeyChallenges, k)
		}
	}
	if len(s.passkeyChallenges) >= passkeyChallengeMax {
		return "", nil, errors.New("挑战数量已达上限，请稍后再试")
	}
	s.passkeyChallenges[chID] = passkeyChallenge{value: raw, exp: now.Add(passkeyChallengeTTL), rp: rp, purpose: purpose}
	return chID, raw, nil
}

// takePasskeyChallenge 取出并删除挑战（单次消费）。不存在/过期/用途或来源不符 → false。
func (s *Server) takePasskeyChallenge(chID, purpose string, rp webauthn.RelyingParty) ([]byte, bool) {
	s.passkeyMu.Lock()
	defer s.passkeyMu.Unlock()
	c, ok := s.passkeyChallenges[chID]
	if !ok {
		return nil, false
	}
	delete(s.passkeyChallenges, chID)
	if time.Now().After(c.exp) || c.purpose != purpose || c.rp.ID != rp.ID || c.rp.Origin != rp.Origin {
		return nil, false
	}
	return c.value, true
}

// rpForRequest 从请求推导依赖方：RP ID 取 Host 的域名部分——WebAuthn 规范
// 里 RP ID 永远不含端口（非标端口的面板同样只绑域名）；Origin 与
// requestBaseURL 同口径（反代场景优先信 X-Forwarded-Proto）。
// 注意：RP ID 绑定域名——Passkey 凭据注册在哪个域名下，就只能在该域名登录。
func (s *Server) rpForRequest(r *http.Request) webauthn.RelyingParty {
	host := r.Host
	if i := strings.LastIndex(host, "]"); i > 0 {
		// IPv6 字面量 [::1]:19080 → RP ID 取 [::1]（含括号的规范写法）
		host = host[:i+1]
	} else if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return webauthn.RelyingParty{ID: host, Origin: requestBaseURL(r)}
}

// ---------- 登录 ceremony（未认证侧） ----------

type passkeyBeginResponse struct {
	ChallengeID      string `json:"challengeId"`
	Challenge        string `json:"challenge"` // base64url
	RPID             string `json:"rpId"`
	UserVerification string `json:"userVerification"`
	Timeout          int    `json:"timeout"`
}

// handlePasskeyLoginBegin 签发登录挑战。
func (s *Server) handlePasskeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if s.currentUser() == nil {
		fail(w, http.StatusServiceUnavailable, "面板登录未启用")
		return
	}
	if len(s.currentUser().Passkeys) == 0 {
		fail(w, http.StatusNotFound, "尚未注册 Passkey，请先用密码登录后在设置中添加")
		return
	}
	rp := s.rpForRequest(r)
	chID, challenge, err := s.newPasskeyChallenge("login", rp)
	if err != nil {
		fail(w, http.StatusInternalServerError, "生成挑战失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, passkeyBeginResponse{
		ChallengeID:      chID,
		Challenge:        webauthn.Base64URLEncode(challenge),
		RPID:             rp.ID,
		UserVerification: "preferred",
		Timeout:          passkeyTimeoutMS,
	})
}

type passkeyLoginRequest struct {
	ChallengeID string `json:"challengeId"`
	ID          string `json:"id"` // base64url(credential ID)
	Type        string `json:"type"`
	Response    struct {
		ClientDataJSON    string `json:"clientDataJSON"`
		AuthenticatorData string `json:"authenticatorData"`
		Signature         string `json:"signature"`
	} `json:"response"`
}

// handlePasskeyLoginFinish 验证断言并铸会话。响应形状与密码登录一致。
func (s *Server) handlePasskeyLoginFinish(w http.ResponseWriter, r *http.Request) {
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
	if ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type"))); !strings.HasPrefix(ct, "application/json") {
		fail(w, http.StatusUnsupportedMediaType, "Content-Type 必须为 application/json")
		return
	}
	body := http.MaxBytesReader(w, r.Body, 64<<10)
	var in passkeyLoginRequest
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if in.ChallengeID == "" || in.ID == "" || in.Type != "public-key" ||
		in.Response.ClientDataJSON == "" || in.Response.AuthenticatorData == "" || in.Response.Signature == "" {
		s.auditAuth(r, "auth.login_failed", "passkey", "login", http.StatusBadRequest, "incomplete passkey response")
		fail(w, http.StatusBadRequest, "断言数据不完整")
		return
	}

	credID, err := webauthn.Base64URLDecode(in.ID)
	if err != nil || len(credID) == 0 {
		s.auditAuth(r, "auth.login_failed", "passkey", "login", http.StatusBadRequest, "bad credential id")
		fail(w, http.StatusBadRequest, "凭据 ID 无效")
		return
	}
	cred, ok := u.FindPasskey(credID)
	if !ok {
		s.auditAuth(r, "auth.login_failed", u.Username, "login", http.StatusUnauthorized, "unknown passkey")
		fail(w, http.StatusUnauthorized, "凭据未注册")
		return
	}
	rp := s.rpForRequest(r)
	challenge, ok := s.takePasskeyChallenge(in.ChallengeID, "login", rp)
	if !ok {
		s.auditAuth(r, "auth.login_failed", u.Username, "login", http.StatusBadRequest, "challenge expired or unknown")
		fail(w, http.StatusBadRequest, "挑战已过期，请重新点击登录")
		return
	}
	cdj, err := webauthn.Base64URLDecode(in.Response.ClientDataJSON)
	if err != nil {
		s.auditAuth(r, "auth.login_failed", u.Username, "login", http.StatusBadRequest, "bad clientDataJSON b64")
		fail(w, http.StatusBadRequest, "clientDataJSON 编码无效")
		return
	}
	authData, err := webauthn.Base64URLDecode(in.Response.AuthenticatorData)
	if err != nil {
		s.auditAuth(r, "auth.login_failed", u.Username, "login", http.StatusBadRequest, "bad authenticatorData b64")
		fail(w, http.StatusBadRequest, "authenticatorData 编码无效")
		return
	}
	sig, err := webauthn.Base64URLDecode(in.Response.Signature)
	if err != nil {
		s.auditAuth(r, "auth.login_failed", u.Username, "login", http.StatusBadRequest, "bad signature b64")
		fail(w, http.StatusBadRequest, "signature 编码无效")
		return
	}
	res, err := webauthn.ParseAssertionResponse(rp, challenge, cdj, authData, sig,
		credID, cred.PublicKey, cred.SignCount)
	if err != nil {
		s.auditAuth(r, "auth.login_failed", u.Username, "login", http.StatusUnauthorized, "passkey assertion invalid: "+err.Error())
		fail(w, http.StatusUnauthorized, "Passkey 验证失败: "+err.Error())
		return
	}

	// 计数更新走 accountMu（与登录/账号修改互斥）。
	s.accountMu.Lock()
	updated, uerr := auth.UpdatePasskeyCounter(s.userPath, s.currentUser(), credID, res.SignCount, time.Now().Unix(), res.UserVerified)
	s.accountMu.Unlock()
	if uerr != nil {
		// 计数更新失败不阻断登录（验证已通过），但必须留痕。
		log.Printf("Passkey 计数更新失败: %v", uerr)
		updated = u
	}

	token, exp, err := s.sessions.Issue(updated.Username, updated.SessionVersion())
	if err != nil {
		log.Printf("签发面板会话失败: %v", err)
		fail(w, http.StatusInternalServerError, "登录失败，请稍后再试")
		return
	}
	s.setSessionCookie(w, r, token, exp)
	ip := clientIP(r, s.trustedProxies)
	s.limiter.resetIP(ip)
	s.auditAuth(r, "auth.login", updated.Username, "passkey", http.StatusOK, "")
	log.Printf("面板登录成功 user=%s ip=%s（Passkey）", updated.Username, ip)
	s.notifyLogin(updated.Username, ip, "passkey")
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"user":          updated.PublicInfo(),
		"expiresAt":     exp.Unix(),
	})
}

// ---------- 注册 ceremony（已认证侧，走 accountChange + 密码确认） ----------

type passkeyRegisterBeginRequest struct {
	Name string `json:"name,omitempty"` // 设备别名，可空
}

// handlePasskeyRegisterBegin 签发注册挑战与 creation options。
func (s *Server) handlePasskeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var in passkeyRegisterBeginRequest
	if !decodeAccountJSON(w, r, &in) {
		return
	}
	u := s.currentUser()
	if u == nil {
		fail(w, http.StatusServiceUnavailable, "面板登录未启用")
		return
	}
	if len(u.Passkeys) >= passkeyMaxPerAccount {
		fail(w, http.StatusBadRequest, "Passkey 数量已达上限（20）")
		return
	}
	rp := s.rpForRequest(r)
	chID, challenge, err := s.newPasskeyChallenge("register", rp)
	if err != nil {
		fail(w, http.StatusInternalServerError, "生成挑战失败: "+err.Error())
		return
	}
	exclude := make([]map[string]any, 0, len(u.Passkeys))
	for _, p := range u.Passkeys {
		exclude = append(exclude, map[string]any{
			"id":   webauthn.Base64URLEncode(p.ID),
			"type": "public-key",
		})
	}
	userID := webauthn.Base64URLEncode([]byte(u.Username)) // 稳定的 user handle（≤64 字节）
	writeJSON(w, http.StatusOK, map[string]any{
		"challengeId":        chID,
		"challenge":          webauthn.Base64URLEncode(challenge),
		"rpId":               rp.ID,
		"userId":             userID,
		"userName":           u.Username,
		"timeout":            passkeyTimeoutMS,
		"excludeCredentials": exclude,
	})
}

type passkeyRegisterFinishRequest struct {
	ChallengeID     string `json:"challengeId"`
	Name            string `json:"name,omitempty"`
	ID              string `json:"id"`
	Type            string `json:"type"`
	CurrentPassword string `json:"currentPassword"`
	Response        struct {
		ClientDataJSON    string `json:"clientDataJSON"`
		AttestationObject string `json:"attestationObject"`
	} `json:"response"`
}

// handlePasskeyRegisterFinish 校验注册响应并落盘凭据。要求当前密码确认
// ——添加登录凭据与改密同级，都需要防"离开座位的人"。
func (s *Server) handlePasskeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var in passkeyRegisterFinishRequest
	rp := s.rpForRequest(r)
	ok := false
	func() {
		body := http.MaxBytesReader(w, r.Body, 64<<10)
		dec := json.NewDecoder(body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			fail(w, http.StatusBadRequest, "bad json: "+err.Error())
			return
		}
		// 先消费挑战（单次）：失败也作废，防止重试窗口被反复利用
		challenge, chOK := s.takePasskeyChallenge(in.ChallengeID, "register", rp)
		if !chOK {
			fail(w, http.StatusBadRequest, "挑战已过期，请重新发起注册")
			return
		}
		if in.ID == "" || in.Type != "public-key" || in.Response.ClientDataJSON == "" || in.Response.AttestationObject == "" {
			fail(w, http.StatusBadRequest, "注册数据不完整")
			return
		}
		credID, err := webauthn.Base64URLDecode(in.ID)
		if err != nil || len(credID) == 0 {
			fail(w, http.StatusBadRequest, "凭据 ID 无效")
			return
		}
		cdj, err := webauthn.Base64URLDecode(in.Response.ClientDataJSON)
		if err != nil {
			fail(w, http.StatusBadRequest, "clientDataJSON 编码无效")
			return
		}
		attObj, err := webauthn.Base64URLDecode(in.Response.AttestationObject)
		if err != nil {
			fail(w, http.StatusBadRequest, "attestationObject 编码无效")
			return
		}
		cred, err := webauthn.ParseCreateResponse(rp, challenge, cdj, attObj)
		if err != nil {
			fail(w, http.StatusBadRequest, "注册校验失败: "+err.Error())
			return
		}
		s.accountChange(w, r, func(w http.ResponseWriter, r *http.Request, current *auth.User) {
			if in.CurrentPassword == "" || !auth.VerifyPassword(current.PasswordHash, in.CurrentPassword) {
				fail(w, http.StatusForbidden, "当前密码不正确")
				return
			}
			updated, err := auth.AddPasskey(s.userPath, current, auth.PasskeyCredential{
				ID:        credID,
				PublicKey: cred.COSEKey,
				SignCount: cred.SignCount,
				Name:      in.Name,
				CreatedAt: time.Now().Unix(),
			})
			if err != nil {
				if strings.Contains(err.Error(), "已注册") {
					fail(w, http.StatusConflict, err.Error())
					return
				}
				log.Printf("保存 Passkey 失败: %v", err)
				fail(w, http.StatusInternalServerError, "保存 Passkey 失败")
				return
			}
			s.setUser(updated)
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "passkeys": len(updated.Passkeys)})
		})
		ok = true
	}()
	_ = ok
}

type passkeyRemoveRequest struct {
	CurrentPassword string `json:"currentPassword"`
	ID              string `json:"id"`
}

// handlePasskeyRemove 吊销一把 Passkey（需当前密码确认）。
// 最后一把 Passkey 可以删——密码 + TOTP 仍是完整的登录方式。
func (s *Server) handlePasskeyRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.accountChange(w, r, func(w http.ResponseWriter, r *http.Request, current *auth.User) {
		var in passkeyRemoveRequest
		if !decodeAccountJSON(w, r, &in) {
			return
		}
		if in.CurrentPassword == "" || !auth.VerifyPassword(current.PasswordHash, in.CurrentPassword) {
			fail(w, http.StatusForbidden, "当前密码不正确")
			return
		}
		credID, err := webauthn.Base64URLDecode(in.ID)
		if err != nil || len(credID) == 0 {
			fail(w, http.StatusBadRequest, "凭据 ID 无效")
			return
		}
		updated, err := auth.RemovePasskey(s.userPath, current, credID)
		if err != nil {
			if errors.Is(err, auth.ErrPasskeyNotFound) {
				fail(w, http.StatusNotFound, err.Error())
				return
			}
			log.Printf("删除 Passkey 失败: %v", err)
			fail(w, http.StatusInternalServerError, "删除失败")
			return
		}
		s.setUser(updated)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "passkeys": len(updated.Passkeys)})
	})
}

// handlePasskeyList 返回已注册的 Passkey 清单（脱敏，不含公钥字节）。
func (s *Server) handlePasskeyList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	u := s.currentUser()
	if u == nil {
		fail(w, http.StatusServiceUnavailable, "面板登录未启用")
		return
	}
	views := make([]auth.PasskeyView, 0, len(u.Passkeys))
	for i, p := range u.Passkeys {
		views = append(views, auth.PasskeyView{
			ID:           webauthn.Base64URLEncode(p.ID),
			Name:         auth.PasskeyDisplayName(p.Name, i),
			CreatedAt:    p.CreatedAt,
			LastUsedAt:   p.LastUsedAt,
			UserVerified: p.UserVerified,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"passkeys": views})
}

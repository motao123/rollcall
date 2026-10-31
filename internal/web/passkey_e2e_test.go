package web

// Passkey 的端到端测试：用 internal/webauthn 的虚拟认证器走完整 ceremony
//（注册 → 会话内列出 → 登录换会话 → 计数回滚拒绝 → 吊销 → 挑战单次消费），
// 覆盖浏览器自动化无法覆盖的加密与状态路径。
//
// rpForRequest 从请求 Host 推导 RP ID/Origin：httptest.NewRequest 的 Host
// 是 "example.com"（无端口），因此虚拟认证器以 example.com/http://example.com 构造。

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/motao123/rollcall/internal/auth"
	"github.com/motao123/rollcall/internal/webauthn"
)

func newPasskeyServer(t *testing.T) (http.Handler, string) {
	t.Helper()
	h, userPath, _ := newAuthWebAt(t, 0)
	return h, userPath
}

func postJSONPasskey(h http.Handler, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func getJSONPasskey(h http.Handler, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// loginViaPassword 用密码登录拿会话 Cookie（注册 Passkey 需要已认证会话）。
func loginViaPassword(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login",
		strings.NewReader(`{"identifier":"demo-admin","password":"`+testAdminPassword+`"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("密码登录失败: %d %s", w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == "rollcall_session" {
			return c
		}
	}
	t.Fatal("登录响应没有会话 Cookie")
	return nil
}

// virtualAuthenticator 是测试内的虚拟认证器：保存私钥与当前计数，
// 注册/断言都走真实协议（challenge 由服务端签发、签名由私钥计算）。
type virtualAuthenticator struct {
	priv      *ecdsa.PrivateKey
	credID    []byte
	signCount uint32
}

func newVirtualAuthenticator(t *testing.T) *virtualAuthenticator {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &virtualAuthenticator{priv: priv}
}

// register 走 begin → create → finish，返回服务端响应。
func (va *virtualAuthenticator) register(t *testing.T, h http.Handler, cookie *http.Cookie, password string) map[string]any {
	t.Helper()
	begin := postJSONPasskey(h, "/api/admin/account/passkey/register/begin", "{}", cookie)
	if begin.Code != http.StatusOK {
		t.Fatalf("register begin 失败: %d %s", begin.Code, begin.Body.String())
	}
	var opts struct {
		ChallengeID string `json:"challengeId"`
		Challenge   string `json:"challenge"`
		RPID        string `json:"rpId"`
		UserID      string `json:"userId"`
		UserName    string `json:"userName"`
	}
	if err := json.Unmarshal(begin.Body.Bytes(), &opts); err != nil {
		t.Fatal(err)
	}
	challenge, err := webauthn.Base64URLDecode(opts.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	rp := testRPFromHost("example.com")
	credID := []byte("test-cred-" + opts.ChallengeID[:8])
	attObj, cdj, _, err := webauthn.BuildAttestationNone(rp.ID, challenge, rp.Origin, credID, va.priv, 0)
	if err != nil {
		t.Fatal(err)
	}
	fin := map[string]any{
		"challengeId":     opts.ChallengeID,
		"name":            "测试设备",
		"id":              webauthn.Base64URLEncode(credID),
		"type":            "public-key",
		"currentPassword": password,
		"response": map[string]any{
			"clientDataJSON":    webauthn.Base64URLEncode([]byte(cdj)),
			"attestationObject": webauthn.Base64URLEncode(attObj),
		},
	}
	fb, _ := json.Marshal(fin)
	w := postJSONPasskey(h, "/api/admin/account/passkey/register/finish", string(fb), cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("register finish 失败: %d %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	va.credID = credID
	return resp
}

// login 走 begin → get → finish，返回登录响应（应含 authenticated:true）与 Set-Cookie。
func (va *virtualAuthenticator) login(t *testing.T, h http.Handler) (*httptest.ResponseRecorder, *http.Cookie) {
	t.Helper()
	begin := postJSONPasskey(h, "/api/auth/passkey/begin", "{}", nil)
	if begin.Code != http.StatusOK {
		t.Fatalf("login begin 失败: %d %s", begin.Code, begin.Body.String())
	}
	var opts struct {
		ChallengeID string `json:"challengeId"`
		Challenge   string `json:"challenge"`
		RPID        string `json:"rpId"`
	}
	if err := json.Unmarshal(begin.Body.Bytes(), &opts); err != nil {
		t.Fatal(err)
	}
	challenge, err := webauthn.Base64URLDecode(opts.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	rp := testRPFromHost("example.com")
	va.signCount++
	authData := webauthn.BuildAuthData(rp.ID, webauthn.FlagUP|webauthn.FlagUV, va.signCount, nil)
	cdj := buildClientDataJSON("webauthn.get", challenge, rp.Origin, false)
	sig, err := webauthn.SignAssertion(va.priv, authData, []byte(cdj))
	if err != nil {
		t.Fatal(err)
	}
	fin := map[string]any{
		"challengeId": opts.ChallengeID,
		"id":          webauthn.Base64URLEncode(va.credID),
		"type":        "public-key",
		"response": map[string]any{
			"clientDataJSON":    webauthn.Base64URLEncode([]byte(cdj)),
			"authenticatorData": webauthn.Base64URLEncode(authData),
			"signature":         webauthn.Base64URLEncode(sig),
		},
	}
	fb, _ := json.Marshal(fin)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/passkey/login", strings.NewReader(string(fb)))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(w, req)
	return w, firstCookie(w)
}

func firstCookie(w *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == "rollcall_session" {
			return c
		}
	}
	return nil
}

func testRPFromHost(host string) webauthn.RelyingParty {
	return webauthn.RelyingParty{ID: host, Origin: "http://" + host}
}

// buildClientDataJSON 构造虚拟认证器的 clientDataJSON（与真实浏览器同形状）。
func buildClientDataJSON(typ string, challenge []byte, origin string, crossOrigin bool) string {
	ch := webauthn.Base64URLEncode(challenge)
	co := ""
	if crossOrigin {
		co = `,"crossOrigin":true`
	}
	return `{"type":"` + typ + `","challenge":"` + ch + `","origin":"` + origin + `"` + co + `}`
}

func TestPasskeyEndToEndRegisterAndLogin(t *testing.T) {
	h, userPath := newPasskeyServer(t)
	va := newVirtualAuthenticator(t)

	// 1) 会话内注册
	cookie := loginViaPassword(t, h)
	resp := va.register(t, h, cookie, testAdminPassword)
	if n, _ := resp["passkeys"].(float64); n != 1 {
		t.Fatalf("注册后应有 1 把，实际 %v", resp["passkeys"])
	}

	// 2) 未认证侧 Passkey 登录 → 换到新会话
	w, newCookie := va.login(t, h)
	var loginResp struct {
		Authenticated bool `json:"authenticated"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &loginResp)
	if !loginResp.Authenticated || newCookie == nil {
		t.Fatalf("Passkey 登录应成功并铸会话: %d %s", w.Code, w.Body.String())
	}

	// 3) 计数回滚：用旧计数签的断言必须被拒绝
	oldCount := va.signCount
	begin := postJSONPasskey(h, "/api/auth/passkey/begin", "{}", nil)
	var opts struct {
		ChallengeID string `json:"challengeId"`
		Challenge   string `json:"challenge"`
		RPID        string `json:"rpId"`
	}
	_ = json.Unmarshal(begin.Body.Bytes(), &opts)
	challenge, _ := webauthn.Base64URLDecode(opts.Challenge)
	rp := testRPFromHost("example.com")
	va.signCount = oldCount - 1
	authData := webauthn.BuildAuthData(rp.ID, webauthn.FlagUP|webauthn.FlagUV, va.signCount, nil)
	cdj := buildClientDataJSON("webauthn.get", challenge, rp.Origin, false)
	sig, err := webauthn.SignAssertion(va.priv, authData, []byte(cdj))
	if err != nil {
		t.Fatal(err)
	}
	fin := map[string]any{
		"challengeId": opts.ChallengeID,
		"id":          webauthn.Base64URLEncode(va.credID),
		"type":        "public-key",
		"response": map[string]any{
			"clientDataJSON":    webauthn.Base64URLEncode([]byte(cdj)),
			"authenticatorData": webauthn.Base64URLEncode(authData),
			"signature":         webauthn.Base64URLEncode(sig),
		},
	}
	fb, _ := json.Marshal(fin)
	w = postJSONPasskey(h, "/api/auth/passkey/login", string(fb), nil)
	if w.Code == http.StatusOK {
		t.Fatal("计数回滚应拒绝登录")
	}

	// 4) 列表 + 吊销
	w = getJSONPasskey(h, "/api/admin/account/passkeys", cookie)
	var list struct {
		Passkeys []auth.PasskeyView `json:"passkeys"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Passkeys) != 1 {
		t.Fatalf("列表应有 1 把，实际 %d", len(list.Passkeys))
	}
	removeBody, _ := json.Marshal(map[string]any{
		"currentPassword": testAdminPassword,
		"id":              list.Passkeys[0].ID,
	})
	w = postJSONPasskey(h, "/api/admin/account/passkey/remove", string(removeBody), cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("吊销失败: %d %s", w.Code, w.Body.String())
	}
	w = getJSONPasskey(h, "/api/admin/account/passkeys", cookie)
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Passkeys) != 0 {
		t.Fatalf("吊销后列表应为空，实际 %d", len(list.Passkeys))
	}
	_ = userPath
}

// TestPasskeyLoginRequiresRegisteredCredential 未注册 Passkey 时 begin 返回 404。
func TestPasskeyLoginRequiresRegisteredCredential(t *testing.T) {
	h, _ := newPasskeyServer(t)
	w := postJSONPasskey(h, "/api/auth/passkey/begin", "{}", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("未注册应 404，实际 %d", w.Code)
	}
}

// TestPasskeyChallengeSingleUse 挑战单次消费：同一 challengeId 第二次 finish 必须失败。
func TestPasskeyChallengeSingleUse(t *testing.T) {
	h, _ := newPasskeyServer(t)
	va := newVirtualAuthenticator(t)
	cookie := loginViaPassword(t, h)
	va.register(t, h, cookie, testAdminPassword)

	begin := postJSONPasskey(h, "/api/auth/passkey/begin", "{}", nil)
	var opts struct {
		ChallengeID string `json:"challengeId"`
		Challenge   string `json:"challenge"`
		RPID        string `json:"rpId"`
	}
	_ = json.Unmarshal(begin.Body.Bytes(), &opts)
	challenge, _ := webauthn.Base64URLDecode(opts.Challenge)
	rp := testRPFromHost("example.com")
	va.signCount++
	authData := webauthn.BuildAuthData(rp.ID, webauthn.FlagUP|webauthn.FlagUV, va.signCount, nil)
	cdj := buildClientDataJSON("webauthn.get", challenge, rp.Origin, false)
	sig, err := webauthn.SignAssertion(va.priv, authData, []byte(cdj))
	if err != nil {
		t.Fatal(err)
	}
	mkFin := func(auth []byte, sg []byte) string {
		fin := map[string]any{
			"challengeId": opts.ChallengeID,
			"id":          webauthn.Base64URLEncode(va.credID),
			"type":        "public-key",
			"response": map[string]any{
				"clientDataJSON":    webauthn.Base64URLEncode([]byte(cdj)),
				"authenticatorData": webauthn.Base64URLEncode(auth),
				"signature":         webauthn.Base64URLEncode(sg),
			},
		}
		b, _ := json.Marshal(fin)
		return string(b)
	}
	w := postJSONPasskey(h, "/api/auth/passkey/login", mkFin(authData, sig), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("首次登录应成功: %d %s", w.Code, w.Body.String())
	}
	// 同一挑战重放（新签名新计数也一样）
	va.signCount++
	authData2 := webauthn.BuildAuthData(rp.ID, webauthn.FlagUP|webauthn.FlagUV, va.signCount, nil)
	sig2, err := webauthn.SignAssertion(va.priv, authData2, []byte(cdj))
	if err != nil {
		t.Fatal(err)
	}
	w = postJSONPasskey(h, "/api/auth/passkey/login", mkFin(authData2, sig2), nil)
	if w.Code == http.StatusOK {
		t.Fatal("同一挑战的第二次 finish 应失败（单次消费）")
	}
}

// TestPasskeyRegisterRequiresPassword 注册必须带当前密码。
func TestPasskeyRegisterRequiresPassword(t *testing.T) {
	h, _ := newPasskeyServer(t)
	va := newVirtualAuthenticator(t)
	cookie := loginViaPassword(t, h)
	begin := postJSONPasskey(h, "/api/admin/account/passkey/register/begin", "{}", cookie)
	var opts struct {
		ChallengeID string `json:"challengeId"`
		Challenge   string `json:"challenge"`
		RPID        string `json:"rpId"`
	}
	_ = json.Unmarshal(begin.Body.Bytes(), &opts)
	challenge, _ := webauthn.Base64URLDecode(opts.Challenge)
	rp := testRPFromHost("example.com")
	credID := []byte("cred-no-password")
	attObj, cdj, _, err := webauthn.BuildAttestationNone(rp.ID, challenge, rp.Origin, credID, va.priv, 0)
	if err != nil {
		t.Fatal(err)
	}
	fin := map[string]any{
		"challengeId":     opts.ChallengeID,
		"id":              webauthn.Base64URLEncode(credID),
		"type":            "public-key",
		"currentPassword": "", // 缺当前密码
		"response": map[string]any{
			"clientDataJSON":    webauthn.Base64URLEncode([]byte(cdj)),
			"attestationObject": webauthn.Base64URLEncode(attObj),
		},
	}
	fb, _ := json.Marshal(fin)
	w := postJSONPasskey(h, "/api/admin/account/passkey/register/finish", string(fb), cookie)
	if w.Code != http.StatusForbidden {
		t.Fatalf("缺当前密码应 403，实际 %d %s", w.Code, w.Body.String())
	}
}

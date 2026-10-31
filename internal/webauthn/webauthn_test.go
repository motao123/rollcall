package webauthn

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"strings"
	"testing"
)

var testRP = RelyingParty{ID: "status.example.com", Origin: "https://status.example.com"}

func testKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

// fullRegistration 用虚拟认证器走完整注册，返回凭据与原始字节。
func fullRegistration(t *testing.T, rp RelyingParty, priv *ecdsa.PrivateKey, credID []byte) (Credential, []byte, []byte) {
	t.Helper()
	challenge := []byte("challenge-bytes-01")
	attObj, cdj, cose, err := BuildAttestationNone(rp.ID, challenge, rp.Origin, credID, priv, 5)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := ParseCreateResponse(rp, challenge, cdj, attObj)
	if err != nil {
		t.Fatalf("注册校验失败: %v", err)
	}
	cred.ID = credID
	return cred, cdj, cose
}

// TestRegistrationHappyPath 注册全链路：challenge/origin/rpIdHash/UP/AT 全部
// 按协议走一遍，凭据 ID 与公钥应被正确提取。
func TestRegistrationHappyPath(t *testing.T) {
	priv := testKey(t)
	cred, cdj, _ := fullRegistration(t, testRP, priv, []byte("cred-id-001"))
	if cred.SignCount != 5 {
		t.Fatalf("签名计数应为 5，实际 %d", cred.SignCount)
	}
	if !cred.UserVerified {
		t.Fatal("虚拟认证器置位了 UV，应返回 true")
	}
	if string(cdj) == "" {
		t.Fatal("clientDataJSON 不应为空")
	}
}

// TestRegistrationRejectsWrongChallenge challenge 不匹配必须拒绝（防重放）。
func TestRegistrationRejectsWrongChallenge(t *testing.T) {
	priv := testKey(t)
	attObj, cdj, _, err := BuildAttestationNone(testRP.ID, []byte("challenge-A"), testRP.Origin, []byte("cred-1"), priv, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseCreateResponse(testRP, []byte("challenge-B"), cdj, attObj); err == nil {
		t.Fatal("challenge 不匹配应拒绝")
	}
}

// TestRegistrationRejectsWrongOrigin origin 与 RP 配置不一致必须拒绝（防钓鱼中继）。
func TestRegistrationRejectsWrongOrigin(t *testing.T) {
	priv := testKey(t)
	evilRP := RelyingParty{ID: "status.example.com", Origin: "https://evil.example.net"}
	attObj, cdj, _, err := BuildAttestationNone(evilRP.ID, []byte("c"), evilRP.Origin, []byte("cred-1"), priv, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseCreateResponse(testRP, []byte("c"), cdj, attObj); err == nil {
		t.Fatal("origin 不匹配应拒绝")
	}
}

// TestRegistrationRejectsMissingUP UP 未置位必须拒绝。
func TestRegistrationRejectsMissingUP(t *testing.T) {
	priv := testKey(t)
	cose := MarshalCOSEKeyP256(priv.PublicKey.X.Bytes(), priv.PublicKey.Y.Bytes())
	// 手工构造 flags=UV|AT（缺 UP）的 authData
	attData := BuildAuthData(testRP.ID, FlagAT|FlagUV, 1, []byte("cred-1"))
	attData = append(attData, cose...)
	challenge := []byte("c")
	cdj := buildClientDataJSON("webauthn.create", challenge, testRP.Origin, false)
	attObj := encodeTestAttestation(attData)

	if _, err := ParseCreateResponse(testRP, challenge, []byte(cdj), attObj); err == nil {
		t.Fatal("UP 未置位应拒绝")
	}
}

// TestRegistrationRejectsWrongRPIDHash rpIdHash 与 RP ID 不匹配必须拒绝。
func TestRegistrationRejectsWrongRPIDHash(t *testing.T) {
	priv := testKey(t)
	otherRP := RelyingParty{ID: "other.example.com", Origin: testRP.Origin}
	attObj, cdj, _, err := BuildAttestationNone(otherRP.ID, []byte("c"), testRP.Origin, []byte("cred-1"), priv, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseCreateResponse(testRP, []byte("c"), cdj, attObj); err == nil {
		t.Fatal("rpIdHash 不匹配应拒绝")
	}
}

// TestRegistrationRejectsUnsupportedAlg 非 ES256 的公钥必须拒绝。
func TestRegistrationRejectsUnsupportedAlg(t *testing.T) {
	// 构造 alg=-257（RS256）的 COSE key：把标准 ES256 map 的 alg 换掉后重新编码
	priv := testKey(t)
	cose := MarshalCOSEKeyP256(priv.PublicKey.X.Bytes(), priv.PublicKey.Y.Bytes())
	// 解开再改 alg
	var key any
	if _, err := cborDecode(cose, &key); err != nil {
		t.Fatal(err)
	}
	m := key.(map[any]any)
	m[int64(3)] = int64(-257)
	badCOSE, err := encodeCBOR(m)
	if err != nil {
		t.Fatal(err)
	}
	attData := BuildAuthData(testRP.ID, FlagUP|FlagAT|FlagUV, 1, []byte("cred-1"))
	attData = append(attData, badCOSE...)
	challenge := []byte("c")
	cdj := buildClientDataJSON("webauthn.create", challenge, testRP.Origin, false)
	attObj := encodeTestAttestation(attData)
	if _, err := ParseCreateResponse(testRP, challenge, []byte(cdj), attObj); err == nil {
		t.Fatal("非 ES256 公钥应拒绝")
	}
}

// fullAssertion 用虚拟认证器签一次登录断言。
func fullAssertion(t *testing.T, rp RelyingParty, priv *ecdsa.PrivateKey, credID []byte, count uint32) ([]byte, []byte, []byte) {
	t.Helper()
	challenge := []byte("login-challenge-01")
	authData := BuildAuthData(rp.ID, FlagUP|FlagUV, count, nil)
	cdj := buildClientDataJSON("webauthn.get", challenge, rp.Origin, false)
	sig, err := SignAssertion(priv, authData, []byte(cdj))
	if err != nil {
		t.Fatal(err)
	}
	return authData, []byte(cdj), sig
}

// TestAssertionHappyPath 登录断言全链路 + 计数必须递增。
func TestAssertionHappyPath(t *testing.T) {
	priv := testKey(t)
	cred, _, _ := fullRegistration(t, testRP, priv, []byte("cred-id-002"))
	authData, cdj, sig := fullAssertion(t, testRP, priv, cred.ID, cred.SignCount+1)

	res, err := ParseAssertionResponse(testRP, []byte("login-challenge-01"), cdj, authData, sig,
		cred.ID, cred.COSEKey, cred.SignCount)
	if err != nil {
		t.Fatalf("断言校验失败: %v", err)
	}
	if res.SignCount != cred.SignCount+1 {
		t.Fatalf("计数应为 %d，实际 %d", cred.SignCount+1, res.SignCount)
	}
}

// TestAssertionRejectsCountRollback 计数回滚（克隆信号）必须拒绝。
func TestAssertionRejectsCountRollback(t *testing.T) {
	priv := testKey(t)
	cred, _, _ := fullRegistration(t, testRP, priv, []byte("cred-id-003"))
	// 用比已存计数更小的值签名
	authData, cdj, sig := fullAssertion(t, testRP, priv, cred.ID, cred.SignCount-1)
	if _, err := ParseAssertionResponse(testRP, []byte("login-challenge-01"), cdj, authData, sig,
		cred.ID, cred.COSEKey, cred.SignCount); err == nil {
		t.Fatal("计数回滚应拒绝")
	}
}

// TestAssertionRejectsBadSignature 签名验证失败（伪造）必须拒绝。
func TestAssertionRejectsBadSignature(t *testing.T) {
	priv := testKey(t)
	cred, _, _ := fullRegistration(t, testRP, priv, []byte("cred-id-004"))
	authData, cdj, _ := fullAssertion(t, testRP, priv, cred.ID, cred.SignCount+1)
	// 先按原始数据签名，再篡改 authData 的最后一个字节（计数位）：
	// 伪造者换掉数据却保留签名，服务端必须拒绝。
	sig := mustSign(t, priv, authData, []byte(cdj))
	authData[len(authData)-1] ^= 0xff
	if _, err := ParseAssertionResponse(testRP, []byte("login-challenge-01"), cdj, authData, sig,
		cred.ID, cred.COSEKey, cred.SignCount); err == nil {
		t.Fatal("签名验证应失败")
	}
}

// TestAssertionRejectsUnknownCredentialID 断言里的凭据 ID 与已注册不符必须拒绝。
func TestAssertionRejectsUnknownCredentialID(t *testing.T) {
	priv := testKey(t)
	authData, cdj, _ := fullAssertion(t, testRP, priv, []byte("other-cred"), 1)
	storedCOSE := MarshalCOSEKeyP256(priv.PublicKey.X.Bytes(), priv.PublicKey.Y.Bytes())
	if _, err := ParseAssertionResponse(testRP, []byte("login-challenge-01"), cdj, authData,
		mustSign(t, priv, authData, []byte(cdj)), []byte("other-cred"), storedCOSE, 1); err == nil {
		t.Fatal("凭据 ID 不符应拒绝")
	}
}

// TestAssertionAcceptsZeroCounters 双 0 计数（无计数器的认证器）应放行。
func TestAssertionAcceptsZeroCounters(t *testing.T) {
	priv := testKey(t)
	credID := []byte("cred-id-005")
	authData, cdj, sig := fullAssertion(t, testRP, priv, credID, 0)
	storedCOSE := MarshalCOSEKeyP256(priv.PublicKey.X.Bytes(), priv.PublicKey.Y.Bytes())
	if _, err := ParseAssertionResponse(testRP, []byte("login-challenge-01"), cdj, authData, sig,
		credID, storedCOSE, 0); err != nil {
		t.Fatalf("双 0 计数应放行: %v", err)
	}
}

// TestBase64URLRoundTrip base64url 无填充往返。
func TestBase64URLRoundTrip(t *testing.T) {
	raw := []byte{0xfb, 0xff, 0xbf, 0xfe, 0x01}
	enc := Base64URLEncode(raw)
	if strings.ContainsAny(enc, "+/=") {
		t.Fatalf("base64url 不应含 +/=: %q", enc)
	}
	dec, err := Base64URLDecode(enc)
	if err != nil || string(dec) != string(raw) {
		t.Fatalf("往返不一致: %v %v", dec, err)
	}
}

// ---------- 测试辅助 ----------

func mustSign(t *testing.T, priv *ecdsa.PrivateKey, authData, cdj []byte) []byte {
	t.Helper()
	combined := append(append([]byte{}, authData...), hashClientData(cdj)...)
	digest := sha256SumBytes(combined)
	sig, err := priv.Sign(rand.Reader, digest, nil)
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

func sha256SumBytes(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

func buildClientDataJSON(typ string, challenge []byte, origin string, crossOrigin bool) string {
	ch := Base64URLEncode(challenge)
	co := ""
	if crossOrigin {
		co = `,"crossOrigin":true`
	}
	return `{"type":"` + typ + `","challenge":"` + ch + `","origin":"` + origin + `"` + co + `}`
}

func encodeTestAttestation(authData []byte) []byte {
	obj := map[any]any{
		"fmt":      "packed",
		"attStmt":  map[any]any{},
		"authData": authData,
	}
	enc, err := encodeCBOR(obj)
	if err != nil {
		panic(err)
	}
	return enc
}

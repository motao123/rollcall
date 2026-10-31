// Package webauthn 实现面板登录所需的 WebAuthn（Level 1）服务端校验，
// 零第三方依赖：CBOR 解码与签名验证全部基于本包 + Go 标准库。
//
// 范围与取舍（都是有意的）：
//   - 只支持 ES256（alg=-7）：现代 Passkey（iCloud/Google/Windows Hello）全部支持
//   - 只校验客户端数据与认证器数据（challenge/origin/rpIdHash/UP/计数），
//     **不校验 attStmt**——Rollcall 不需要知道认证器型号，WebAuthn L1 明确允许
//     忽略证明声明；这把 CBOR 校验面缩小到"结构正确"即可
//   - 注册只接受包含认证凭据数据（AT 标志位）的响应
//   - BuildAttestationNone / SignAssertion 是"虚拟认证器"路径，供集成测试
//     与诊断工具在无真实硬件时走完整协议
package webauthn

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
)

// RelyingParty 是 WebAuthn 的依赖方标识。
// ID 是纯域名（不含 scheme/端口）；Origin 是浏览器提交的完整来源
// （含 scheme；非默认端口带端口）。两者都必须与注册时完全一致。
type RelyingParty struct {
	ID     string
	Origin string
}

// Credential 是注册成功后提取的公钥凭据。
type Credential struct {
	ID           []byte // authenticator 生成的凭据 ID
	COSEKey      []byte // COSE 公钥的 CBOR 原始字节
	SignCount    uint32
	UserVerified bool // 用户校验（生物识别/PIN）是否通过
}

// Assertion 是登录断言验证的结果。
type Assertion struct {
	SignCount    uint32
	UserVerified bool
}

// ---------- base64url（WebAuthn JSON 编码用，无填充） ----------

func Base64URLEncode(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func Base64URLDecode(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}

// ---------- 客户端数据 ----------

type clientData struct {
	Type        string `json:"type"`
	Challenge   string `json:"challenge"`
	Origin      string `json:"origin"`
	CrossOrigin bool   `json:"crossOrigin,omitempty"`
}

func parseClientData(rp RelyingParty, wantType string, challenge []byte, clientDataJSON []byte) error {
	var cd clientData
	if err := json.Unmarshal(clientDataJSON, &cd); err != nil {
		return fmt.Errorf("clientDataJSON 不是合法 JSON: %w", err)
	}
	if cd.Type != wantType {
		return fmt.Errorf("clientData.type = %q，期望 %q", cd.Type, wantType)
	}
	ch, err := Base64URLDecode(cd.Challenge)
	if err != nil {
		return fmt.Errorf("clientData.challenge 不是合法 base64url: %w", err)
	}
	if string(ch) != string(challenge) {
		return errors.New("challenge 不匹配（可能是过期或被替换的挑战）")
	}
	if cd.Origin != rp.Origin {
		return fmt.Errorf("clientData.origin = %q，期望 %q", cd.Origin, rp.Origin)
	}
	if cd.CrossOrigin {
		return errors.New("clientData.crossOrigin = true（跨源 iframe 内发起，拒绝）")
	}
	return nil
}

// ---------- 认证器数据（authData） ----------

const (
	FlagUP = 0x01 // 用户在场
	FlagUV = 0x04 // 用户校验（生物识别/PIN）
	FlagAT = 0x40 // 附带认证凭据数据（注册响应必有）
)

type authData struct {
	RPIDHash  []byte
	Flags     byte
	SignCount uint32
	// AT 置位时才有下面这些：
	AAGUID  []byte
	CredID  []byte
	COSEKey []byte // COSE 公钥的 CBOR 原始字节
}

func parseAuthData(raw []byte) (authData, error) {
	var ad authData
	if len(raw) < 37 {
		return ad, fmt.Errorf("authData 长度 %d 不足 37 字节", len(raw))
	}
	ad.RPIDHash = raw[:32]
	ad.Flags = raw[32]
	ad.SignCount = binary.BigEndian.Uint32(raw[33:37])
	rest := raw[37:]
	if ad.Flags&FlagAT != 0 {
		if len(rest) < 18 {
			return ad, errors.New("authData 声明包含凭据数据但长度不足")
		}
		ad.AAGUID = rest[:16]
		idLen := int(binary.BigEndian.Uint16(rest[16:18]))
		if idLen == 0 || idLen > 1023 {
			return ad, fmt.Errorf("凭据 ID 长度 %d 超出范围", idLen)
		}
		if len(rest) < 18+idLen {
			return ad, errors.New("authData 的凭据 ID 被截断")
		}
		ad.CredID = rest[18 : 18+idLen]
		rest = rest[18+idLen:]
		// 剩余部分是 COSE 公钥（CBOR）：解码一次拿到原始字节
		var key any
		n, err := cborDecode(rest, &key)
		if err != nil {
			return ad, fmt.Errorf("COSE 公钥解码失败: %w", err)
		}
		ad.COSEKey = rest[:n]
	}
	return ad, nil
}

func checkRPIDHash(ad authData, rpID string) error {
	sum := sha256.Sum256([]byte(rpID))
	if string(ad.RPIDHash) != string(sum[:]) {
		return fmt.Errorf("rpIdHash 与 RP ID %q 不匹配（注册与登录的域名必须一致）", rpID)
	}
	return nil
}

func requireFlags(ad authData, needUP, needAT bool) (bool, error) {
	if ad.Flags&FlagUP == 0 {
		return false, errors.New("用户在场标志（UP）未置位")
	}
	uv := ad.Flags&FlagUV != 0
	if needAT && ad.Flags&FlagAT == 0 {
		return false, errors.New("缺少认证凭据数据（AT 标志未置位）")
	}
	return uv, nil
}

// ---------- 注册 ----------

// ParseCreateResponse 校验注册响应并提取公钥凭据。
func ParseCreateResponse(rp RelyingParty, challenge []byte, clientDataJSON, attObj []byte) (Credential, error) {
	var cred Credential
	if err := parseClientData(rp, "webauthn.create", challenge, clientDataJSON); err != nil {
		return cred, err
	}
	var obj any
	if _, err := cborDecode(attObj, &obj); err != nil {
		return cred, fmt.Errorf("attestationObject 解码失败: %w", err)
	}
	m, ok := obj.(map[any]any)
	if !ok {
		return cred, errors.New("attestationObject 顶层不是 CBOR map")
	}
	fmtS, _ := m["fmt"].(string)
	if fmtS == "" {
		return cred, errors.New("attestationObject 缺少 fmt")
	}
	authBytes, ok := m["authData"].([]byte)
	if !ok {
		return cred, errors.New("attestationObject 缺少 authData")
	}
	// attStmt 有意跳过：本面板不需要认证器型号信任（见包注释）。
	ad, err := parseAuthData(authBytes)
	if err != nil {
		return cred, err
	}
	if err := checkRPIDHash(ad, rp.ID); err != nil {
		return cred, err
	}
	uv, err := requireFlags(ad, true, true)
	if err != nil {
		return cred, err
	}
	if err := validateCOSEKey(ad.COSEKey); err != nil {
		return cred, err
	}
	cred.COSEKey = ad.COSEKey
	cred.SignCount = ad.SignCount
	cred.UserVerified = uv
	return cred, nil
}

// validateCOSEKey 校验 COSE Key：只接受 ES256（kty=EC2, alg=ES256, crv=P-256），
// 坐标必须在曲线上。
func validateCOSEKey(cose []byte) error {
	var key any
	if _, err := cborDecode(cose, &key); err != nil {
		return fmt.Errorf("COSE key 解码失败: %w", err)
	}
	m, ok := key.(map[any]any)
	if !ok {
		return errors.New("COSE key 不是 map")
	}
	kty, _ := toInt(m[int64(1)])
	alg, _ := toInt(m[int64(3)])
	crv, _ := toInt(m[int64(-1)])
	x, _ := m[int64(-2)].([]byte)
	y, _ := m[int64(-3)].([]byte)
	if kty != 2 {
		return fmt.Errorf("只支持 EC2 公钥（kty=%v）", kty)
	}
	if alg != -7 {
		return fmt.Errorf("只支持 ES256（alg=%v）", alg)
	}
	if crv != 1 {
		return errors.New("只支持 P-256 曲线且坐标必须为 32 字节")
	}
	// 部分软件认证器 / WebCrypto 编码会把坐标的前导零剥掉（31 字节），
	// 按大端左填充回 32 字节再验；空坐标或超长仍然拒绝。
	var err error
	if x, err = padCoordinate(x); err != nil {
		return fmt.Errorf("公钥 X 坐标: %w", err)
	}
	if y, err = padCoordinate(y); err != nil {
		return fmt.Errorf("公钥 Y 坐标: %w", err)
	}
	// 坐标必须在曲线上：拒绝随手构造的字节（负路径覆盖）
	pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: bytesToBigInt(x), Y: bytesToBigInt(y)}
	if !pub.Curve.IsOnCurve(pub.X, pub.Y) {
		return errors.New("公钥坐标不在 P-256 曲线上")
	}
	return nil
}

// padCoordinate 把剥掉前导零的坐标（如 31 字节）左填充回 32 字节。
func padCoordinate(b []byte) ([]byte, error) {
	switch {
	case len(b) == 0:
		return nil, errors.New("坐标为空")
	case len(b) > 32:
		return nil, errors.New("坐标超过 32 字节")
	case len(b) == 32:
		return b, nil
	}
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out, nil
}

// ---------- 登录断言 ----------

// ParseAssertionResponse 校验登录断言。
// storedCOSE/storedCount 来自账号上已注册的凭据。
func ParseAssertionResponse(rp RelyingParty, challenge []byte, clientDataJSON, authData, sigDER, credID, storedCOSE []byte, storedCount uint32) (Assertion, error) {
	var res Assertion
	if err := parseClientData(rp, "webauthn.get", challenge, clientDataJSON); err != nil {
		return res, err
	}
	ad, err := parseAuthData(authData)
	if err != nil {
		return res, err
	}
	if err := checkRPIDHash(ad, rp.ID); err != nil {
		return res, err
	}
	uv, err := requireFlags(ad, true, false)
	if err != nil {
		return res, err
	}
	// 凭据 ID 的比对在 web 层完成（断言响应的 id 字段 ↔ 账号上已注册的凭据）：
	// 登录断言的 authData 不含凭据数据（无 AT 标志），签名绑定已保证
	// 这段 authData 出自持有该私钥的认证器。
	// 计数策略：双方都为 0（部分认证器不实现计数器）时跳过校验；
	// 否则必须严格递增——回滚 = 凭据被克隆的强信号。
	if storedCount != 0 || ad.SignCount != 0 {
		if ad.SignCount <= storedCount {
			return res, fmt.Errorf("签名计数回滚（%d → %d），凭据可能已被克隆", storedCount, ad.SignCount)
		}
	}
	if err := VerifyP256(storedCOSE, append(append([]byte{}, authData...), hashClientData(clientDataJSON)...), sigDER); err != nil {
		return res, err
	}
	res.SignCount = ad.SignCount
	res.UserVerified = uv
	return res, nil
}

// VerifyP256 用 COSE 公钥验证 ES256 签名（DER 编码，SHA-256 摘要）。
func VerifyP256(cose, data, sigDER []byte) error {
	var key any
	if _, err := cborDecode(cose, &key); err != nil {
		return fmt.Errorf("COSE key 解码失败: %w", err)
	}
	m, ok := key.(map[any]any)
	if !ok {
		return errors.New("COSE key 不是 map")
	}
	x, _ := m[int64(-2)].([]byte)
	y, _ := m[int64(-3)].([]byte)
	if len(x) != 32 || len(y) != 32 {
		return errors.New("COSE key 坐标无效")
	}
	pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: bytesToBigInt(x), Y: bytesToBigInt(y)}
	digest := sha256.Sum256(data)
	if !ecdsa.VerifyASN1(pub, digest[:], sigDER) {
		return errors.New("签名验证失败")
	}
	return nil
}

// ---------- 虚拟认证器（集成测试与诊断工具用） ----------

// BuildAuthData 构造认证器数据：rpIdHash + flags + 计数（+ 可选附加字节）。
func BuildAuthData(rpID string, flags byte, signCount uint32, extra []byte) []byte {
	out := make([]byte, 0, 37+len(extra))
	sum := sha256.Sum256([]byte(rpID))
	out = append(out, sum[:]...)
	out = append(out, flags)
	var cnt [4]byte
	binary.BigEndian.PutUint32(cnt[:], signCount)
	out = append(out, cnt[:]...)
	out = append(out, extra...)
	return out
}

// BuildAttestationNone 构造 fmt="none" 的注册响应 attestation 对象。
// 一个诚实的虚拟认证器：凭据 ID 与 COSE 公钥由调用方的密钥对给出。
// 认证凭据数据的形状与真实认证器一致：AAGUID(16) + ID 长度(2) + ID + COSE key。
func BuildAttestationNone(rpID string, challenge []byte, origin string, credID []byte, priv *ecdsa.PrivateKey, signCount uint32) (attObj, clientDataJSON, coseKey []byte, err error) {
	// 坐标定宽 32 字节（规范行为）：big.Int.Bytes() 会剥前导零，
	// 约 1/128 的密钥会产出 31 字节坐标——那是部分软件认证器才会有的编码。
	xb := make([]byte, 32)
	yb := make([]byte, 32)
	priv.PublicKey.X.FillBytes(xb)
	priv.PublicKey.Y.FillBytes(yb)
	coseKey = MarshalCOSEKeyP256(xb, yb)
	if len(credID) == 0 {
		sum := sha256.Sum256(coseKey)
		credID = sum[:32]
	}
	// attested credential data：AAGUID 用全零占位（虚拟认证器无型号）。
	attested := make([]byte, 16)
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(credID)))
	attested = append(attested, l[:]...)
	attested = append(attested, credID...)
	attested = append(attested, coseKey...)
	authData := BuildAuthData(rpID, FlagUP|FlagAT|FlagUV, signCount, attested)
	clientDataJSON, _ = json.Marshal(map[string]any{
		"type":      "webauthn.create",
		"challenge": Base64URLEncode(challenge),
		"origin":    origin,
	})
	m := map[any]any{
		"fmt":      "none",
		"attStmt":  map[any]any{},
		"authData": authData,
	}
	attObj, err = encodeCBOR(m)
	return attObj, clientDataJSON, coseKey, err
}

// SignAssertion 用虚拟认证器的私钥签一段登录断言（覆盖 authData || SHA256(clientDataJSON)）。
// 供集成测试与诊断工具在无真实硬件时走完整协议。
func SignAssertion(priv *ecdsa.PrivateKey, authData, clientDataJSON []byte) (sigDER []byte, err error) {
	digest := sha256.Sum256(append(append([]byte{}, authData...), hashClientData(clientDataJSON)...))
	return priv.Sign(rand.Reader, digest[:], nil)
}

// ---------- COSE 编码（虚拟认证器与测试用） ----------

// MarshalCOSEKeyP256 把 P-256 公钥坐标编码为 COSE Key 的 CBOR 字节
// （kty=2 EC2, alg=-7 ES256, crv=1 P-256, x, y）。
func MarshalCOSEKeyP256(x, y []byte) []byte {
	out, _ := encodeCBOR(map[any]any{
		int64(1):  int64(2),  // kty: EC2
		int64(3):  int64(-7), // alg: ES256
		int64(-1): int64(1),  // crv: P-256
		int64(-2): x,
		int64(-3): y,
	})
	return out
}

// ---------- CBOR 解码（RFC 8949 子集） ----------

var errCBORTruncated = errors.New("CBOR 数据被截断")

// cborDecode 解码第一个 CBOR 项，返回值与消耗的字节数。
// 结果类型：nil / bool / int64 / uint64 / float64 / []byte / string /
// []any / map[any]any（键可能是 int64 或 string）。
func cborDecode(b []byte, out *any) (int, error) {
	if len(b) == 0 {
		return 0, errCBORTruncated
	}
	mt := b[0] >> 5
	ai := b[0] & 0x1f
	val, n, err := cborHead(b, mt, ai)
	if err != nil {
		return 0, err
	}
	switch mt {
	case 0:
		if val > math.MaxInt64 {
			*out = val
			return n, nil
		}
		*out = int64(val)
		return n, nil
	case 1:
		if val > math.MaxInt64 {
			return 0, fmt.Errorf("CBOR 负整数超出范围")
		}
		*out = -1 - int64(val)
		return n, nil
	case 2:
		if ai == 31 {
			return decodeIndefinite(b, n, out, reflectKindBytes)
		}
		if uint64(len(b)) < uint64(n)+val {
			return 0, errCBORTruncated
		}
		*out = append([]byte{}, b[n:uint64(n)+val]...)
		return n + int(val), nil
	case 3:
		if ai == 31 {
			return decodeIndefinite(b, n, out, reflectKindText)
		}
		if uint64(len(b)) < uint64(n)+val {
			return 0, errCBORTruncated
		}
		*out = string(b[n : uint64(n)+val])
		return n + int(val), nil
	case 4:
		if ai == 31 {
			return 0, errors.New("CBOR 不定长数组不受支持")
		}
		items := make([]any, 0, int(val))
		pos := n
		for i := uint64(0); i < val; i++ {
			var item any
			used, err := cborDecode(b[pos:], &item)
			if err != nil {
				return 0, err
			}
			items = append(items, item)
			pos += used
		}
		*out = items
		return pos, nil
	case 5:
		if ai == 31 {
			return 0, errors.New("CBOR 不定长 map 不受支持")
		}
		m := make(map[any]any, int(val))
		pos := n
		for i := uint64(0); i < val; i++ {
			var k, v any
			used, err := cborDecode(b[pos:], &k)
			if err != nil {
				return 0, err
			}
			pos += used
			used, err = cborDecode(b[pos:], &v)
			if err != nil {
				return 0, err
			}
			pos += used
			m[k] = v
		}
		*out = m
		return pos, nil
	case 6:
		// 标签：跳过标签值，解码被包裹的项
		var item any
		used, err := cborDecode(b[n:], &item)
		if err != nil {
			return 0, err
		}
		*out = item
		return n + used, nil
	case 7:
		switch ai {
		case 20:
			*out = false
		case 21:
			*out = true
		case 22, 23:
			*out = nil
		case 25:
			*out = float64(halfToFloat(uint16(val)))
		case 26:
			*out = float64(math.Float32frombits(uint32(val)))
		case 27:
			*out = math.Float64frombits(val)
		default:
			return 0, fmt.Errorf("不支持的 CBOR 简单值 %d", val)
		}
		return n, nil
	}
	return 0, fmt.Errorf("不支持的 CBOR 主类型 %d", mt)
}

// cborHead 解析初始字节与扩展长度，返回（无符号值、头部字节数）。
func cborHead(b []byte, mt, ai byte) (uint64, int, error) {
	n := 1
	switch ai {
	case 24, 25, 26, 27:
		size := 1 << (ai - 24)
		if len(b) < n+size {
			return 0, 0, errCBORTruncated
		}
		var val uint64
		for _, c := range b[n : n+size] {
			val = val<<8 | uint64(c)
		}
		return val, n + size, nil
	case 28, 29, 30:
		return 0, 0, fmt.Errorf("CBOR 保留的附加信息 %d", ai)
	case 31:
		if mt == 0 || mt == 1 {
			return 0, 0, errors.New("CBOR 整数不允许不定长")
		}
		return 0, n, nil
	default:
		return uint64(ai), n, nil
	}
}

// decodeIndefinite 处理不定长字节串/文本串（0xff 结束）。
func decodeIndefinite(b []byte, head int, out *any, kind reflectKind) (int, error) {
	pos := head
	var bytesBuf []byte
	var textBuf strings.Builder
	for {
		if pos >= len(b) {
			return 0, errCBORTruncated
		}
		if b[pos] == 0xff {
			pos++
			break
		}
		mt := b[pos] >> 5
		if int(mt) != int(kind) {
			return 0, errors.New("CBOR 不定长串的内嵌块类型不符")
		}
		var chunk any
		used, err := cborDecode(b[pos:], &chunk)
		if err != nil {
			return 0, err
		}
		switch kind {
		case reflectKindBytes:
			bytesBuf = append(bytesBuf, chunk.([]byte)...)
		case reflectKindText:
			textBuf.WriteString(chunk.(string))
		}
		pos += used
	}
	if kind == reflectKindBytes {
		*out = bytesBuf
	} else {
		*out = textBuf.String()
	}
	return pos, nil
}

type reflectKind int

const (
	reflectKindBytes reflectKind = 2
	reflectKindText  reflectKind = 3
)

func halfToFloat(h uint16) float64 {
	sign := uint64(h>>15) & 1
	exp := uint64(h>>10) & 0x1f
	frac := uint64(h) & 0x3ff
	var f float64
	switch exp {
	case 0:
		f = math.Ldexp(float64(frac), -24)
	case 31:
		if frac == 0 {
			f = math.Inf(1)
		} else {
			f = math.NaN()
		}
	default:
		f = math.Ldexp(float64(frac)+1024, int(exp)-25)
	}
	if sign == 1 {
		f = -f
	}
	return f
}

func toInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case uint64:
		if n <= math.MaxInt64 {
			return int64(n), true
		}
	}
	return 0, false
}

// bytesToBigInt 把大端字节转成大整数（坐标转公钥用）。
func hashClientData(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// bytesToBigInt 把大端字节转成大整数（坐标转公钥用）。
func bytesToBigInt(b []byte) *big.Int {
	out := new(big.Int)
	for _, c := range b {
		out.Lsh(out, 8)
		out.Or(out, big.NewInt(int64(c)))
	}
	return out
}

package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// 实现 RFC 6238 TOTP（SHA-1、6 位、30 秒），与主流身份验证器兼容。
// 不引入第三方依赖，避免为一个纯算法增加供应链面。

var totpEnc = base32.StdEncoding.WithPadding(base32.NoPadding)

const (
	totpStep   = 30
	totpDigits = 6
)

// NewTOTPSecret 生成 160 位随机密钥（base32 无填充）。
func NewTOTPSecret() (string, error) {
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("生成 TOTP 密钥失败: %w", err)
	}
	return totpEnc.EncodeToString(raw), nil
}

func decodeTOTPSecret(secret string) ([]byte, error) {
	s := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(secret), " ", ""))
	if s == "" {
		return nil, errors.New("密钥为空")
	}
	b, err := totpEnc.DecodeString(s)
	// RFC 4226/6238 要求共享密钥至少 128 位。旧实现只要求 10 字节（80 位），
	// 手工导入或改写过 admin-user.json 的弱密钥会被静默接受并用于派生 HMAC。
	if err != nil || len(b) < 16 {
		return nil, errors.New("密钥格式无效（至少 128 位）")
	}
	return b, nil
}

// TOTPURL 生成 otpauth:// 链接，供前端渲染二维码。
func TOTPURL(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", "6")
	q.Set("period", "30")
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// totpAt 计算指定时刻的动态码。
func totpAt(secret string, t time.Time) (string, error) {
	key, err := decodeTOTPSecret(secret)
	if err != nil {
		return "", err
	}
	counter := uint64(t.Unix() / totpStep)
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := (uint32(sum[off])&0x7f)<<24 |
		uint32(sum[off+1])<<16 |
		uint32(sum[off+2])<<8 |
		uint32(sum[off+3])
	return fmt.Sprintf("%06d", code%1_000_000), nil
}

// TOTPCode 计算指定时刻的动态码。
// 供测试与运维排查（例如核对手机与服务端时间是否一致）使用：
// 算法本身只依赖密钥与时间，导出它不会削弱安全性。
func TOTPCode(secret string, now time.Time) (string, error) {
	return totpAt(secret, now)
}

// ErrTOTPReplayed 表示该动态码的时间步已经被消费过（重放）。
var ErrTOTPReplayed = errors.New("动态验证码已使用，请等待下一个")

// VerifyTOTPStep 校验动态码，返回命中的时间步（Unix/30）。
// 允许前后各一个时间窗的时钟偏差；调用方必须把返回的 step 持久化，
// 并拒绝 step <= 上次已消费值 的请求（RFC 6238 §5.2）。
func VerifyTOTPStep(secret, code string, now time.Time) (int64, bool) {
	code = strings.TrimSpace(code)
	if len(code) != totpDigits {
		return 0, false
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	for _, offset := range []int{-1, 0, 1} {
		at := now.Add(time.Duration(offset) * totpStep * time.Second)
		want, err := totpAt(secret, at)
		if err != nil {
			return 0, false
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return at.Unix() / totpStep, true
		}
	}
	return 0, false
}

// VerifyTOTP 只做校验、不返回时间步。
// 保留它是为了不破坏既有调用；需要防重放的路径必须用 VerifyTOTPStep。
func VerifyTOTP(secret, code string, now time.Time) bool {
	_, ok := VerifyTOTPStep(secret, code, now)
	return ok
}

// MarkTOTPStep 把时间步记为已消费并落盘（原子替换，0600）。
// 返回 ErrTOTPReplayed 表示该步已用过，调用方应拒绝本次登录。
func MarkTOTPStep(path string, current *User, step int64) (*User, error) {
	if current == nil {
		return nil, errors.New("管理员账号未初始化")
	}
	if step <= current.LastTOTPStep {
		return nil, ErrTOTPReplayed
	}
	next := *current
	next.LastTOTPStep = step
	if err := saveUser(path, &next); err != nil {
		return nil, err
	}
	return &next, nil
}

// UpdateTOTP 保存 TOTP 密钥与启用状态并落盘。
// 启用/停用都会递增会话版本，使其他浏览器上的会话立即失效。
func UpdateTOTP(path string, current *User, secret string, enabled bool) (*User, error) {
	if current == nil {
		return nil, errors.New("管理员账号未初始化")
	}
	next := *current
	next.TOTPSecret = secret
	next.TOTPEnabled = enabled
	if enabled {
		if _, err := decodeTOTPSecret(secret); err != nil {
			return nil, err
		}
	}
	changed := next.TOTPEnabled != current.TOTPEnabled || next.TOTPSecret != current.TOTPSecret
	if !changed {
		return current, nil
	}
	// 仅保存尚未启用的待确认密钥时不换会话版本：用户可能先扫码再确认，
	// 中间还有一次页面请求，提前失效会让流程自己把自己登出。
	// 启用/停用要换版本；**已启用状态下轮换密钥也必须换**：
	// 否则旧密钥派生的动态码仍能登录，等于轮换没生效。
	// 比较归一化后的值，避免大小写/空格差异造成"看起来换了其实没换"的误判。
	if next.TOTPEnabled != current.TOTPEnabled ||
		(next.TOTPEnabled && !strings.EqualFold(strings.TrimSpace(next.TOTPSecret), strings.TrimSpace(current.TOTPSecret))) {
		next.UpdatedAt = nextVersion(current.UpdatedAt)
	}
	if err := saveUser(path, &next); err != nil {
		return nil, err
	}
	return &next, nil
}

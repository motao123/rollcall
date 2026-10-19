// Package auth 管理面板管理员账号与会话。
//
// 设计约束：
//   - 账号只来自服务端配置文件/环境变量，不硬编码在代码或前端
//   - 落盘只保存 PBKDF2-SHA256 哈希，绝不保存明文密码
//   - 会话是 HMAC 签名的 HttpOnly Cookie，不把凭证放进 URL
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	// Iterations 是 PBKDF2 迭代次数。600k 与 OWASP 对
	// PBKDF2-HMAC-SHA256 的建议一致；登录一次约数百毫秒。
	Iterations = 600_000

	saltSize    = 16
	keySize     = 32
	minHashIter = 100_000
	maxHashIter = 2_000_000

	MinPasswordLen = 12
	maxPasswordLen = 1024
)

var b64 = base64.RawStdEncoding

// HashPassword 返回格式为 pbkdf2-sha256$迭代$盐$哈希 的可持久化字符串。
func HashPassword(password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	salt := make([]byte, saltSize)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("生成密码盐失败: %w", err)
	}
	key := deriveKey([]byte(password), salt, Iterations, keySize)
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", Iterations,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// ValidatePassword 拒绝空密码与过短密码。上限避免异常输入拖垮哈希计算。
func ValidatePassword(password string) error {
	if password == "" {
		return errors.New("密码不能为空")
	}
	if len(password) > maxPasswordLen {
		return fmt.Errorf("密码不能超过 %d 字节", maxPasswordLen)
	}
	if len([]rune(password)) < MinPasswordLen {
		return fmt.Errorf("密码至少需要 %d 个字符", MinPasswordLen)
	}
	return nil
}

// VerifyPassword 以常量时间比较密码哈希。格式非法时返回 false。
func VerifyPassword(encoded, password string) bool {
	if len(password) > maxPasswordLen {
		// 与 HashPassword 同一上限：不可能被哈希的输入也无需计算。
		return false
	}
	iter, salt, want, err := parseHash(encoded)
	if err != nil {
		return false
	}
	got := deriveKey([]byte(password), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// ValidateHash 校验哈希格式与参数范围，不计算哈希。
func ValidateHash(encoded string) error {
	_, _, _, err := parseHash(encoded)
	return err
}

func parseHash(encoded string) (int, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return 0, nil, nil, errors.New("密码哈希格式无效")
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < minHashIter || iter > maxHashIter {
		return 0, nil, nil, errors.New("密码哈希迭代次数无效")
	}
	salt, err := b64.DecodeString(parts[2])
	if err != nil || len(salt) != saltSize {
		return 0, nil, nil, errors.New("密码哈希盐值无效")
	}
	key, err := b64.DecodeString(parts[3])
	if err != nil || len(key) < 16 || len(key) > 64 {
		return 0, nil, nil, errors.New("密码哈希长度无效")
	}
	return iter, salt, key, nil
}

// deriveKey 是 PBKDF2-HMAC-SHA256（RFC 8018），用标准库实现以避免外部依赖。
func deriveKey(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hLen := prf.Size()
	blocks := (keyLen + hLen - 1) / hLen
	out := make([]byte, 0, blocks*hLen)
	u := make([]byte, hLen)
	t := make([]byte, hLen)
	var idx [4]byte
	for block := 1; block <= blocks; block++ {
		prf.Reset()
		_, _ = prf.Write(salt)
		binary.BigEndian.PutUint32(idx[:], uint32(block))
		_, _ = prf.Write(idx[:])
		u = prf.Sum(u[:0])
		copy(t, u)
		for n := 1; n < iter; n++ {
			prf.Reset()
			_, _ = prf.Write(u)
			u = prf.Sum(u[:0])
			for i := range t {
				t[i] ^= u[i]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

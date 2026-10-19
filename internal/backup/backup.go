// Package backup 实现配置备份的密码加密。
//
// 格式：magic(5) + 迭代次数(4, big-endian) + 盐(16) + nonce(12) + AES-256-GCM 密文。
// 头部作为附加认证数据参与 GCM 校验，篡改迭代次数或盐会直接解密失败。
package backup

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/motao/rollcall/internal/auth"
)

const (
	magic    = "RCBK1"
	iter     = 210_000
	saltLen  = 16
	nonceLen = 12
	// maxIter 防止恶意备份文件用超高迭代次数拖垮 CPU。
	maxIter = 600_000
)

// Encrypt 使用密码加密明文。
func Encrypt(plain []byte, password string) ([]byte, error) {
	if password == "" {
		return nil, errors.New("备份密码不能为空")
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("生成备份盐失败: %w", err)
	}
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("生成备份随机数失败: %w", err)
	}
	key := auth.DeriveKey([]byte(password), salt, iter, 32)
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	header := make([]byte, 0, len(magic)+4+saltLen)
	header = append(header, magic...)
	header = binary.BigEndian.AppendUint32(header, iter)
	header = append(header, salt...)
	out := append(header, nonce...)
	return gcm.Seal(out, nonce, plain, header), nil
}

// Decrypt 解密备份；密码错误或文件被篡改时返回错误。
func Decrypt(data []byte, password string) ([]byte, error) {
	hLen := len(magic) + 4 + saltLen
	if len(data) < hLen+nonceLen+16 || string(data[:len(magic)]) != magic {
		return nil, errors.New("不是有效的 Rollcall 备份文件")
	}
	n := binary.BigEndian.Uint32(data[len(magic) : len(magic)+4])
	if n < 100_000 || n > maxIter {
		return nil, errors.New("备份文件的加密参数无效")
	}
	header := data[:hLen]
	salt := data[len(magic)+4 : hLen]
	nonce := data[hLen : hLen+nonceLen]
	key := auth.DeriveKey([]byte(password), salt, int(n), 32)
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, data[hLen+nonceLen:], header)
	if err != nil {
		return nil, errors.New("备份密码错误或文件已损坏")
	}
	return plain, nil
}

// Checksum 返回备份内容的 SHA-256 前缀，便于在界面上核对同一份文件。
func Checksum(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:8])
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

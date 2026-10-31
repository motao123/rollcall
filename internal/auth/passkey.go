package auth

// Passkey 凭据的存取。与 TOTP 走同一套账号文件与并发模式：
// 调用方（web 层）持有 accountMu 并传入最新读到的 current，这里改完落盘。
// 凭据 ID / 公钥不经过这里校验——协议层校验在 internal/webauthn，
// 这里只负责在账号文件上增删改。

import (
	"errors"
	"strconv"
	"time"

	"github.com/motao123/rollcall/internal/sanitize"
)

// ErrPasskeyNotFound 表示凭据 ID 不存在于账号上。
var ErrPasskeyNotFound = errors.New("Passkey 凭据不存在")

// AddPasskey 注册一把新凭据。ID 重复视为同一把（重放注册响应），直接拒绝。
func AddPasskey(path string, current *User, cred PasskeyCredential) (*User, error) {
	if current == nil {
		return nil, errors.New("管理员账号未初始化")
	}
	if len(cred.ID) == 0 || len(cred.PublicKey) == 0 {
		return nil, errors.New("凭据数据不完整")
	}
	for _, p := range current.Passkeys {
		if string(p.ID) == string(cred.ID) {
			return nil, errors.New("该凭据已注册过")
		}
	}
	next := *current
	next.Passkeys = append(append([]PasskeyCredential{}, current.Passkeys...), cred)
	if err := saveUser(path, &next); err != nil {
		return nil, err
	}
	return &next, nil
}

// RemovePasskey 按凭据 ID 吊销。传入凭据 ID 的原始字节。
func RemovePasskey(path string, current *User, credID []byte) (*User, error) {
	if current == nil {
		return nil, errors.New("管理员账号未初始化")
	}
	kept := make([]PasskeyCredential, 0, len(current.Passkeys))
	found := false
	for _, p := range current.Passkeys {
		if string(p.ID) == string(credID) {
			found = true
			continue
		}
		kept = append(kept, p)
	}
	if !found {
		return nil, ErrPasskeyNotFound
	}
	next := *current
	next.Passkeys = kept
	if err := saveUser(path, &next); err != nil {
		return nil, err
	}
	return &next, nil
}

// UpdatePasskeyCounter 更新签名计数与最后使用时间（每次成功登录调用）。
// 不递增会话版本：登录本身刚签发了新会话，把其他浏览器踢下线没有意义。
// 计数回滚（同 ID 报出更小的计数）在这里拒绝——那是凭据被克隆的强信号。
func UpdatePasskeyCounter(path string, current *User, credID []byte, count uint32, usedAt int64, userVerified bool) (*User, error) {
	if current == nil {
		return nil, errors.New("管理员账号未初始化")
	}
	next := *current
	changed := false
	for i := range next.Passkeys {
		if string(next.Passkeys[i].ID) != string(credID) {
			continue
		}
		if count != 0 && count <= next.Passkeys[i].SignCount {
			return nil, errors.New("签名计数回滚，凭据可能已被克隆")
		}
		next.Passkeys[i].SignCount = count
		next.Passkeys[i].LastUsedAt = usedAt
		next.Passkeys[i].UserVerified = userVerified
		changed = true
		break
	}
	if !changed {
		return nil, ErrPasskeyNotFound
	}
	if err := saveUser(path, &next); err != nil {
		return nil, err
	}
	return &next, nil
}

// PasskeyView 是管理端展示用的脱敏凭据（不含公钥字节）。
type PasskeyView struct {
	ID           string `json:"id"` // base64url(credential ID)
	Name         string `json:"name,omitempty"`
	CreatedAt    int64  `json:"createdAt"`
	LastUsedAt   int64  `json:"lastUsedAt,omitempty"`
	UserVerified bool   `json:"userVerified,omitempty"`
}

// PasskeyDisplayName 生成用户可见别名：留空时按序号取名。
func PasskeyDisplayName(name string, index int) string {
	name = sanitize.Text(name, 64)
	if name != "" {
		return name
	}
	return "Passkey " + strconv.Itoa(index+1)
}

// FindPasskey 返回指定 ID 的凭据（内存查找，不落盘）。
func (u *User) FindPasskey(credID []byte) (*PasskeyCredential, bool) {
	for i := range u.Passkeys {
		if string(u.Passkeys[i].ID) == string(credID) {
			return &u.Passkeys[i], true
		}
	}
	return nil, false
}

var _ = time.Second // 保留 time 导入位：后续挑战过期时间如需在 auth 层处理再启用

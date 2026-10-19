package auth

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const (
	userSchema   = 1
	maxUserFile  = 64 << 10
	maxEmailLen  = 254
	maxNameRunes = 32
)

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{1,31}$`)

// User 是面板唯一的管理员账号。
type User struct {
	Schema       int    `json:"schema"`
	Username     string `json:"username"`
	Email        string `json:"email,omitempty"`
	PasswordHash string `json:"passwordHash"`
	UpdatedAt    int64  `json:"updatedAt"` // 秒级时间戳；改密后旧会话立即失效
}

// ValidateUsername 校验登录用户名。
func ValidateUsername(name string) error {
	if !usernameRe.MatchString(name) {
		return errors.New("用户名需为 2–32 位字母、数字、点、下划线或连字符，且以字母或数字开头")
	}
	return nil
}

// ValidateEmail 校验可选邮箱；空字符串表示不启用邮箱登录。
func ValidateEmail(email string) error {
	if email == "" {
		return nil
	}
	if len(email) > maxEmailLen {
		return errors.New("邮箱过长")
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || !strings.EqualFold(addr.Address, email) {
		return errors.New("邮箱格式无效")
	}
	return nil
}

// PublicInfo 返回可安全下发给前端的字段。
func (u *User) PublicInfo() map[string]string {
	return map[string]string{"username": u.Username, "email": u.Email}
}

// SessionVersion 用于绑定会话与当前密码版本。
func (u *User) SessionVersion() int64 {
	if u.UpdatedAt <= 0 {
		return 1
	}
	return u.UpdatedAt
}

// Authenticate 接受用户名或邮箱。无论标识是否匹配都会执行一次哈希校验，
// 避免通过响应时间判断账号是否存在。
func (u *User) Authenticate(identifier, password string) bool {
	ident := strings.TrimSpace(identifier)
	match := strings.EqualFold(ident, u.Username) ||
		(u.Email != "" && strings.EqualFold(ident, u.Email))
	ok := VerifyPassword(u.PasswordHash, password)
	return match && ok
}

// LoadUser 读取并严格校验管理员配置文件。
func LoadUser(path string) (*User, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > maxUserFile {
		return nil, errors.New("管理员配置文件必须是小于 64KB 的普通文件")
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		// 哈希本身不是明文，但不应让同机其他用户读到盐与哈希。
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, fmt.Errorf("管理员配置文件权限过宽且无法收紧: %w", err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var u User
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&u); err != nil {
		return nil, fmt.Errorf("管理员配置文件格式错误: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("管理员配置文件包含多余内容")
	}
	if err := u.validate(); err != nil {
		return nil, err
	}
	return &u, nil
}

func (u *User) validate() error {
	if u.Schema != userSchema {
		return fmt.Errorf("不支持的管理员配置 schema: %d", u.Schema)
	}
	u.Username = strings.TrimSpace(u.Username)
	u.Email = strings.ToLower(strings.TrimSpace(u.Email))
	if err := ValidateUsername(u.Username); err != nil {
		return err
	}
	if err := ValidateEmail(u.Email); err != nil {
		return err
	}
	if err := ValidateHash(u.PasswordHash); err != nil {
		return err
	}
	if u.UpdatedAt < 0 {
		return errors.New("管理员配置更新时间无效")
	}
	return nil
}

// LoadOrCreateUser 在配置文件不存在时创建账号，存在时只读取。
//
// initialPassword 仅在首次创建或 reset=true 时使用；正常启动时不会
// 用环境变量覆盖已保存的密码，避免误改导致现有会话与凭证失效。
// 返回的 generatedPassword 非空表示密码是本次随机生成的，调用方应只提示一次。
func LoadOrCreateUser(path, username, email, initialPassword string, reset bool) (u *User, created bool, generatedPassword string, err error) {
	existing, loadErr := LoadUser(path)
	if loadErr == nil {
		if !reset {
			return existing, false, "", nil
		}
		if initialPassword == "" {
			return nil, false, "", errors.New("重置密码时必须提供 -admin-password 或 ROLLCALL_ADMIN_PASSWORD")
		}
		if err := ValidatePassword(initialPassword); err != nil {
			return nil, false, "", err
		}
		hash, err := HashPassword(initialPassword)
		if err != nil {
			return nil, false, "", err
		}
		existing.PasswordHash = hash
		now := time.Now().Unix()
		if now <= existing.UpdatedAt {
			// 秒级版本号必须严格递增，否则同一秒内的重置不会让旧会话失效。
			now = existing.UpdatedAt + 1
		}
		existing.UpdatedAt = now
		if err := saveUser(path, existing); err != nil {
			return nil, false, "", err
		}
		return existing, false, "", nil
	}
	if !errors.Is(loadErr, os.ErrNotExist) {
		return nil, false, "", loadErr
	}

	username = strings.TrimSpace(username)
	if username == "" {
		username = "admin"
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if err := ValidateUsername(username); err != nil {
		return nil, false, "", err
	}
	if err := ValidateEmail(email); err != nil {
		return nil, false, "", err
	}
	if initialPassword == "" {
		initialPassword, err = generatePassword()
		if err != nil {
			return nil, false, "", err
		}
		generatedPassword = initialPassword
	}
	hash, err := HashPassword(initialPassword)
	if err != nil {
		return nil, false, "", err
	}
	u = &User{
		Schema:       userSchema,
		Username:     username,
		Email:        email,
		PasswordHash: hash,
		UpdatedAt:    time.Now().Unix(),
	}
	if err := saveUser(path, u); err != nil {
		return nil, false, "", err
	}
	return u, true, generatedPassword, nil
}

func generatePassword() (string, error) {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("生成初始密码失败: %w", err)
	}
	return b64.EncodeToString(raw), nil
}

func saveUser(path string, u *User) error {
	if err := u.validate(); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建管理员配置目录失败: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".admin-user-*")
	if err != nil {
		return fmt.Errorf("写入管理员配置失败: %w", err)
	}
	name := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(name)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("设置管理员配置权限失败: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		cleanup()
		return fmt.Errorf("写入管理员配置失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("落盘管理员配置失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("关闭管理员配置失败: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("替换管理员配置失败: %w", err)
	}
	return nil
}

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadAdminTokenRejectsWeakFile 是回归测试。
//
// 「至少 16 字符」原来只校验 -admin-token 参数。把 data/admin.token
// 截断成一个短字符串就能绕过这条强度要求，而该文件是管理 API 的唯一凭据。
func TestLoadAdminTokenRejectsWeakFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "admin.token")

	if err := os.WriteFile(p, []byte("short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateAdminToken(dir); err == nil {
		t.Fatal("过短的令牌文件应被拒绝")
	} else if !strings.Contains(err.Error(), "字符") {
		t.Fatalf("错误信息应说明长度要求: %v", err)
	}

	strong := "0123456789abcdef0123456789abcdef"
	if err := os.WriteFile(p, []byte(strong+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadOrCreateAdminToken(dir)
	if err != nil || got != strong {
		t.Fatalf("合法令牌应被接受: %q %v", got, err)
	}

	// 文件不存在时自动生成，且长度满足策略
	dir2 := t.TempDir()
	gen, err := loadOrCreateAdminToken(dir2)
	if err != nil {
		t.Fatalf("生成令牌: %v", err)
	}
	if len(gen) < minAdminTokenLen {
		t.Fatalf("生成的令牌过短: %q", gen)
	}
}

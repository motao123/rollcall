package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/motao/rollcall/internal/selfupdate"
)

func writeAsset(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, make([]byte, selfupdate.MinAssetSize), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestBuildManifestRejectsDuplicateAssetKeys 防止重复 -asset 静默覆盖。
//
// 同一平台键写两次时 map 只留最后一个，签出的清单看起来完全正常，
// 却少了一个架构 —— 该平台此后收不到任何更新。
func TestBuildManifestRejectsDuplicateAssetKeys(t *testing.T) {
	dir := t.TempDir()
	p := writeAsset(t, dir, "rollcall-agent-linux-amd64")

	_, err := buildManifest(
		[]string{"linux-amd64=" + p, "linux-amd64=" + p},
		"v1.7.0", 1700000000, "https://example.com/releases/v1.7.0", false)
	if err == nil || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("重复平台键应报错，实际 err=%v", err)
	}

	m, err := buildManifest(
		[]string{"linux-amd64=" + p, "linux-arm64=" + p},
		"v1.7.0", 1700000000, "https://example.com/releases/v1.7.0", false)
	if err != nil {
		t.Fatalf("不同平台键应通过: %v", err)
	}
	if len(m.Assets) != 2 {
		t.Fatalf("资产数 = %d，期望 2", len(m.Assets))
	}
	if m.Version != "1.7.0" {
		t.Fatalf("版本应去掉 v 前缀，实际 %q", m.Version)
	}
}

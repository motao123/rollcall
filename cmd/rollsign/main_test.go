package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/motao123/rollcall/internal/selfupdate"
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
// TestCheckOutputPathRejectsKeyAndAssets 防止 -out 覆盖私钥或资产。
//
// os.WriteFile 会直接截断目标文件：把 -out 写成私钥路径会毁掉唯一的签名密钥，
// 写成资产路径会让已计算的 SHA-256 与实际文件不一致，而命令仍报「已写入」。
func TestCheckOutputPathRejectsKeyAndAssets(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "rollsign.key")
	if err := os.WriteFile(key, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	asset := writeAsset(t, dir, "rollcall-agent-linux-amd64")

	if err := checkOutputPath(key, key, []string{"linux-amd64=" + asset}); err == nil {
		t.Fatal("-out 指向私钥应被拒绝")
	}
	if err := checkOutputPath(asset, key, []string{"linux-amd64=" + asset}); err == nil {
		t.Fatal("-out 指向资产应被拒绝")
	}
	// 语法不同但指向同一文件
	if err := checkOutputPath(filepath.Join(dir, ".", "rollsign.key"), key, nil); err == nil {
		t.Fatal("等价路径应被识别")
	}
	if err := checkOutputPath(filepath.Join(dir, "manifest.json"), key,
		[]string{"linux-amd64=" + asset}); err != nil {
		t.Fatalf("正常输出路径不应被拒绝: %v", err)
	}
}

func TestBuildManifestRejectsDuplicateAssetKeys(t *testing.T) {
	dir := t.TempDir()
	p := writeAsset(t, dir, "rollcall-agent-linux-amd64")
	p2 := writeAsset(t, dir, "rollcall-agent-linux-arm64")

	_, err := buildManifest(
		[]string{"linux-amd64=" + p, "linux-amd64=" + p},
		"v1.7.0", 1700000000, "https://example.com/releases/v1.7.0", false)
	if err == nil || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("重复平台键应报错，实际 err=%v", err)
	}

	m, err := buildManifest(
		[]string{"linux-amd64=" + p, "linux-arm64=" + p2},
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

	// 不同目录下的同名文件会生成相同下载地址，必须拒绝
	sub := filepath.Join(dir, "arm64")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	p3 := writeAsset(t, sub, "rollcall-agent-linux-amd64")
	if _, err := buildManifest(
		[]string{"linux-amd64=" + p, "linux-arm64=" + p3},
		"v1.7.0", 1700000000, "https://example.com/releases/v1.7.0", false); err == nil {
		t.Fatal("同名资产会生成冲突的下载地址，应报错")
	}
}

package themepack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"aurora", "a", "a1-b2", strings.Repeat("a", 32)} {
		if !ValidName(ok) {
			t.Fatalf("%q 应合法", ok)
		}
	}
	for _, bad := range []string{"", "Aurora", "../etc", "a/b", ".hidden", "a_b", strings.Repeat("a", 33)} {
		if ValidName(bad) {
			t.Fatalf("%q 应非法", bad)
		}
	}
}

func TestLoadAndDiscover(t *testing.T) {
	root := Root(t.TempDir())
	writeFile(t, filepath.Join(root, "aurora", ManifestName),
		`{"name":"aurora","title":"极光","description":"深色玻璃","version":"1.0","author":"ops"}`)
	writeFile(t, filepath.Join(root, "aurora", CSSName), "body{}")
	writeFile(t, filepath.Join(root, "aurora", JSName), "console.log(1)")

	p, err := Load(root, "aurora")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Title != "极光" || p.Version != "1.0" || !p.HasCSS || !p.HasJS || p.Dir == "" {
		t.Fatalf("加载结果异常: %+v", p)
	}

	// 合法目录、缺清单目录、非法目录名、普通文件都应出现在扫描结果里，
	// 但普通文件被忽略。
	writeFile(t, filepath.Join(root, "bare", CSSName), "body{}")
	writeFile(t, filepath.Join(root, "Bad Name", ManifestName), `{}`)
	writeFile(t, filepath.Join(root, "README.txt"), "not a theme")

	entries := Discover(root)
	if len(entries) != 3 {
		t.Fatalf("应扫描到 3 个目录，实际 %d: %+v", len(entries), entries)
	}
	if entries[0].Name != "Bad Name" || entries[0].Err == nil {
		t.Fatalf("非法目录名应带错误: %+v", entries[0])
	}
	if entries[1].Name != "aurora" || entries[1].Err != nil {
		t.Fatalf("aurora 应有效: %+v", entries[1])
	}
	if entries[2].Name != "bare" || entries[2].Err == nil {
		t.Fatalf("缺清单应带错误: %+v", entries[2])
	}
}

func TestLoadRejectsBadPacks(t *testing.T) {
	root := Root(t.TempDir())
	cases := []struct {
		name    string
		files   map[string]string
		wantErr string
	}{
		{"missing", nil, "缺少 theme.json"},
		{"unknown", map[string]string{ManifestName: `{"title":"x","extra":1}`, CSSName: "a{}"}, "unknown field"},
		{"trailing", map[string]string{ManifestName: `{} {}`, CSSName: "a{}"}, "单个 JSON 对象"},
		{"badname", map[string]string{ManifestName: `{"name":"other"}`, CSSName: "a{}"}, "必须与目录名"},
		{"empty", map[string]string{ManifestName: `{}`}, "至少需要"},
		{"toolong", map[string]string{ManifestName: `{"title":"` + strings.Repeat("长", 81) + `"}`, CSSName: "a{}"}, "80 个字符"},
		{"jsdir", map[string]string{ManifestName: `{}`, CSSName: "a{}"}, "必须是普通文件"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(root, tc.name)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			for f, body := range tc.files {
				writeFile(t, filepath.Join(dir, f), body)
			}
			if tc.name == "jsdir" {
				if err := os.MkdirAll(filepath.Join(dir, JSName), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			_, err := Load(root, tc.name)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("应通过，实际 %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误应包含 %q，实际 %v", tc.wantErr, err)
			}
		})
	}

	if _, err := Load(root, "../etc"); err == nil {
		t.Fatal("非法主题名必须拒绝")
	}
}

func TestLoadRejectsSymlink(t *testing.T) {
	root := Root(t.TempDir())
	dir := filepath.Join(root, "linked")
	writeFile(t, filepath.Join(dir, ManifestName), `{}`)
	outside := filepath.Join(t.TempDir(), "secret.css")
	writeFile(t, outside, "body{}")
	link := filepath.Join(dir, CSSName)
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("当前系统不支持符号链接: %v", err)
	}
	// ★ 必须回读确认链接真的建出来了：某些受限环境（Windows 无特权、
	// 沙箱拦截）里 os.Symlink 会**返回 nil 却没有创建任何东西**，
	// 于是 Load 报的是"至少需要 status.css"，测试会误判成产品缺陷。
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Skip("当前环境未真正创建符号链接，跳过")
	}
	if _, err := Load(root, "linked"); err == nil || !strings.Contains(err.Error(), "符号链接不接受") {
		t.Fatalf("符号链接必须拒绝，实际 %v", err)
	}
}

func TestDiscoverMissingRoot(t *testing.T) {
	if entries := Discover(filepath.Join(t.TempDir(), "nope")); len(entries) != 0 {
		t.Fatalf("不存在的根目录应返回空列表: %+v", entries)
	}
}

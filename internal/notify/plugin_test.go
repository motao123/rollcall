package notify

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writePluginFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("写入 %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil { // 绕过 umask
		t.Fatalf("chmod %s: %v", path, err)
	}
}

func pluginManifestJSON(t *testing.T, dir, name string, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化清单: %v", err)
	}
	path := filepath.Join(dir, name)
	writePluginFile(t, path, string(b), 0o644)
	return path
}

func TestPluginLoadAndSend(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows 没有 POSIX 权限位：chmod 设置的可执行位/0600 读回来仍是 666，
		// 这里断言的是 Linux 上的安全语义，跳过而不是让整个仓库在 Windows 上红灯。
		t.Skip("Windows 无 POSIX 权限位，跳过文件权限断言")
	}
	dir := t.TempDir()
	outFile := filepath.Join(t.TempDir(), "request.json")
	envFile := filepath.Join(t.TempDir(), "env.txt")

	writePluginFile(t, filepath.Join(dir, "hook.sh"), "#!/bin/sh\ncat > \"$OUT_FILE\"\nenv > \"$ENV_FILE\"\n", 0o755)
	pluginManifestJSON(t, dir, "hook.json", pluginManifest{
		Type:    "script",
		Label:   "测试脚本",
		Desc:    "把请求写入文件",
		Command: "hook.sh",
		Timeout: 5,
		Fields: []Field{
			{Key: "secretKey", Label: "密钥", Type: "password", Required: true, Secret: true},
			{Key: "target", Label: "目标", Type: "text"},
		},
		Env: []string{"OUT_FILE=" + outFile, "ENV_FILE=" + envFile, "CUSTOM=1"},
	})

	if err := LoadPlugins(dir); err != nil {
		t.Fatalf("LoadPlugins: %v", err)
	}
	t.Cleanup(func() { _ = LoadPlugins("") })

	if PluginCount() != 1 {
		t.Fatalf("插件数量 %d，want 1", PluginCount())
	}
	found := false
	for _, d := range Defs() {
		if d.Type == "script" {
			found = true
			if d.Label != "测试脚本" || len(d.Fields) != 2 {
				t.Fatalf("插件定义异常: %+v", d)
			}
		}
	}
	if !found {
		t.Fatal("Defs 未包含插件")
	}

	cfg := &Config{
		Enabled: true, MinSeverity: "warn",
		Channels: []ChannelConfig{{
			ID: "p1", Type: "script", Name: "脚本", Enabled: true,
			Options: map[string]string{"secretKey": "s3cr3t", "target": "room"},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("插件通道应通过校验: %v", err)
	}
	if got := cfg.Redacted().Channels[0].Options["secretKey"]; !isMasked(got) {
		t.Fatalf("插件密钥未脱敏: %q", got)
	}

	t.Setenv("PLUGIN_LEAK_MARKER", "should-not-appear")
	results := Send(cfg, Message{
		Title: "CPU 过高", NodeName: "prod-01", RuleName: "CPU", Severity: "warn", TS: 1700000000,
	})
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("插件投递失败: %+v", results)
	}

	raw, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("读取插件请求: %v", err)
	}
	var req pluginRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("解析插件请求: %v", err)
	}
	if req.Channel.Options["secretKey"] != "s3cr3t" || req.Channel.Options["target"] != "room" {
		t.Fatalf("插件未收到通道参数: %+v", req.Channel)
	}
	if !strings.Contains(req.Subject, "告警") || req.Message.Title != "CPU 过高" {
		t.Fatalf("插件请求内容异常: %+v", req)
	}

	env, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("读取插件环境: %v", err)
	}
	envStr := string(env)
	if !strings.Contains(envStr, "CUSTOM=1") {
		t.Fatalf("清单 env 未传入: %s", envStr)
	}
	if strings.Contains(envStr, "PLUGIN_LEAK_MARKER") {
		t.Fatalf("子进程不应继承服务端环境: %s", envStr)
	}
}

func TestPluginFailures(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows 没有 POSIX 权限位：chmod 设置的可执行位/0600 读回来仍是 666，
		// 这里断言的是 Linux 上的安全语义，跳过而不是让整个仓库在 Windows 上红灯。
		t.Skip("Windows 无 POSIX 权限位，跳过文件权限断言")
	}
	dir := t.TempDir()
	writePluginFile(t, filepath.Join(dir, "fail.sh"), "#!/bin/sh\necho 'boom on stderr' >&2\nexit 3\n", 0o755)
	writePluginFile(t, filepath.Join(dir, "slow.sh"), "#!/bin/sh\nsleep 5\n", 0o755)
	pluginManifestJSON(t, dir, "fail.json", pluginManifest{
		Type: "failer", Label: "失败插件", Command: "fail.sh", Timeout: 5,
	})
	pluginManifestJSON(t, dir, "slow.json", pluginManifest{
		Type: "slowpoke", Label: "超时插件", Command: "slow.sh", Timeout: 1,
	})
	if err := LoadPlugins(dir); err != nil {
		t.Fatalf("LoadPlugins: %v", err)
	}
	t.Cleanup(func() { _ = LoadPlugins("") })

	cfg := func(typ string) *Config {
		return &Config{Enabled: true, MinSeverity: "warn", Channels: []ChannelConfig{{
			ID: "p1", Type: typ, Name: typ, Enabled: true,
		}}}
	}
	res := Send(cfg("failer"), Message{Title: "t", Severity: "warn"})
	if len(res) != 1 || res[0].OK || !strings.Contains(res[0].Error, "boom") {
		t.Fatalf("失败插件结果异常: %+v", res)
	}
	res = Send(cfg("slowpoke"), Message{Title: "t", Severity: "warn"})
	if len(res) != 1 || res[0].OK || !strings.Contains(res[0].Error, "超时") {
		t.Fatalf("超时插件结果异常: %+v", res)
	}
}

func TestPluginRejectsUnsafeManifests(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows 没有 POSIX 权限位：chmod 设置的可执行位/0600 读回来仍是 666，
		// 这里断言的是 Linux 上的安全语义，跳过而不是让整个仓库在 Windows 上红灯。
		t.Skip("Windows 无 POSIX 权限位，跳过文件权限断言")
	}
	cases := []struct {
		name    string
		setup   func(t *testing.T, dir string)
		wantErr string
	}{
		{
			name: "覆盖内置类型",
			setup: func(t *testing.T, dir string) {
				writePluginFile(t, filepath.Join(dir, "hook.sh"), "#!/bin/sh\ntrue\n", 0o755)
				pluginManifestJSON(t, dir, "hook.json", pluginManifest{
					Type: "webhook", Label: "伪装", Command: "hook.sh",
				})
			},
			wantErr: "冲突",
		},
		{
			name: "路径逃逸",
			setup: func(t *testing.T, dir string) {
				pluginManifestJSON(t, dir, "hook.json", pluginManifest{
					Type: "safe", Label: "逃逸", Command: "../outside.sh",
				})
			},
			wantErr: "越出",
		},
		{
			name: "符号链接逃逸",
			setup: func(t *testing.T, dir string) {
				if err := os.Symlink("/bin/sh", filepath.Join(dir, "link.sh")); err != nil {
					t.Skipf("无法创建符号链接: %v", err)
				}
				pluginManifestJSON(t, dir, "hook.json", pluginManifest{
					Type: "linked", Label: "链接", Command: "link.sh",
				})
			},
			wantErr: "符号链接",
		},
		{
			name: "未知字段",
			setup: func(t *testing.T, dir string) {
				writePluginFile(t, filepath.Join(dir, "hook.sh"), "#!/bin/sh\ntrue\n", 0o755)
				writePluginFile(t, filepath.Join(dir, "hook.json"),
					`{"type":"extra","label":"X","command":"hook.sh","css":"body{}"}`, 0o644)
			},
			wantErr: "清单格式错误",
		},
		{
			name: "不可执行",
			setup: func(t *testing.T, dir string) {
				writePluginFile(t, filepath.Join(dir, "hook.sh"), "#!/bin/sh\ntrue\n", 0o644)
				pluginManifestJSON(t, dir, "hook.json", pluginManifest{
					Type: "noexec", Label: "不可执行", Command: "hook.sh",
				})
			},
			wantErr: "可执行权限",
		},
		{
			name: "重复类型",
			setup: func(t *testing.T, dir string) {
				writePluginFile(t, filepath.Join(dir, "hook.sh"), "#!/bin/sh\ntrue\n", 0o755)
				for _, name := range []string{"a.json", "b.json"} {
					pluginManifestJSON(t, dir, name, pluginManifest{
						Type: "dup", Label: "重复", Command: "hook.sh",
					})
				}
			},
			wantErr: "重复",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(t, dir)
			err := LoadPlugins(dir)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err=%v，want 包含 %q", err, tc.wantErr)
			}
			_ = LoadPlugins("")
		})
	}
}

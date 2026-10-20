package web

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/motao/rollcall/internal/store"
)

// TestAgentInstallScriptIsSafeAndPublic 是「一条命令接入」的回归测试。
//
// 脚本必须：不含任何密钥（node id/token 由参数传入）、按架构分发、
// HTTP 非回环时要求显式 --allow-insecure 才继续（下载的二进制会以 root 执行）、
// 下载前在临时路径验证可执行。任何人可拉取（脚本本身无秘密）。
func TestAgentInstallScriptIsSafeAndPublic(t *testing.T) {
	srv := New(store.New(t.TempDir()), nil, nil, Config{})

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/agent/install.sh", nil))
	if rec.Code != 200 {
		t.Fatalf("install.sh = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// 不配置 Basic Auth 时也不受影响：脚本必须能匿名获取
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "shellscript") {
		t.Fatalf("Content-Type = %q", ct)
	}
	// 脚本模板必须已填充控制端地址（%[1]s 不应残留）
	if strings.Contains(body, "%[1]s") {
		t.Fatal("脚本模板未填充 server 地址")
	}
	// 明文 HTTP 非回环必须显式放行：否则只打印警告就下载并 root 执行，
	// 误用公网 http 时攻击者可直接替换二进制。
	for _, want := range []string{
		"ALLOW_INSECURE=1", "--allow-insecure",
		`"$ALLOW_INSECURE" != "1"`, " -allow-insecure",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("脚本缺少明文 HTTP 的显式放行逻辑 %q", want)
		}
	}
	// 脚本不能把 token 写死：只能来自参数 $3
	if !strings.Contains(body, "TOKEN=$3") || !strings.Contains(body, `"$TOKEN"`) {
		t.Fatal("token 应通过参数传入并被引用")
	}
	// 安装前必须在临时路径验证二进制可执行
	if !strings.Contains(body, "$TMP\" -version") {
		t.Fatal("脚本缺少下载后可执行性验证")
	}
	// 下载内容必须确认是 Agent 本体，避免把错误页/别的程序装成正式二进制
	if !strings.Contains(body, "不是 Rollcall Agent") || !strings.Contains(body, "rollcall-agent\\ *") {
		t.Fatal("脚本缺少下载文件身份校验")
	}
	// 控制端地址必须带 scheme（会原样写入 JSON）
	if !strings.Contains(body, "https://?*)") {
		t.Fatal("脚本缺少控制端地址格式校验")
	}
	// 无 systemd 的容器里也要尽量用专用用户运行，而不是默默 root
	if !strings.Contains(body, "runuser -u \"$RUN_AS\" --") ||
		!strings.Contains(body, "无法以 $RUN_AS 运行") {
		t.Fatal("无 systemd 分支缺少降权/告警")
	}
	// 仅存在 systemctl 客户端不代表 systemd 是 PID 1（容器很常见）
	if !strings.Contains(body, "/run/systemd/system") {
		t.Fatal("脚本只检查 systemctl 是否存在，容器里会走错分支")
	}
	// 重复安装/升级必须停掉旧进程，且只能按 /proc/<pid>/exe 精确匹配。
	// pkill -f 会误杀命令行里恰好包含二进制路径的无关进程。
	if strings.Contains(body, "pkill -f \"$BIN\"") || strings.Contains(body, "pgrep -f \"$BIN\"") {
		t.Fatal("不能用 pgrep/pkill -f 匹配进程：会误杀命令行包含该路径的进程")
	}
	for _, want := range []string{"agent_pids", "/proc/[0-9]*/exe", "/proc/"} {
		if !strings.Contains(body, want) {
			t.Fatalf("脚本缺少按 exe 精确匹配的进程管理逻辑 %q", want)
		}
	}
	// 下载后必须校验服务器给出的 SHA-256
	for _, want := range []string{"agent/download.sha256", "SHA-256 不匹配", "sha256sum"} {
		if !strings.Contains(body, want) {
			t.Fatalf("脚本缺少下载校验逻辑 %q", want)
		}
	}
	// 生成的 unit 必须与 deploy/ 模板一致，不能少掉关键加固项
	for _, want := range []string{
		"PrivateDevices=true", "RestrictNamespaces=true",
		"RestrictAddressFamilies=AF_INET AF_INET6",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("生成的 unit 缺少加固项 %q", want)
		}
	}
	if strings.Contains(body, "AF_UNIX") {
		t.Fatal("Agent 不需要 Unix socket，生成的 unit 不应放行 AF_UNIX")
	}
	// 无 systemd 的升级必须自动重启，而不是只提示手动重启
	if !strings.Contains(body, "未检测到 rollcall-agent.service，重启后台进程") ||
		!strings.Contains(body, "start_agent_bg") {
		t.Fatal("无 systemd 的升级分支缺少自动重启")
	}
	// 配置值必须转义后写入 JSON，且拒绝控制字符
	for _, want := range []string{"json_escape", "[[:cntrl:]]", "SERVER_JSON"} {
		if !strings.Contains(body, want) {
			t.Fatalf("脚本缺少 JSON 参数处理 %q", want)
		}
	}
	// 临时文件不能使用可预测路径，也不能落在可能 noexec 的 /tmp
	if !strings.Contains(body, "mktemp -d /root/.rollcall-install") {
		t.Fatal("脚本缺少安全临时目录")
	}
	// 升级模式：不接收 node token，读取现有配置、保留 .prev、按原属主替换
	for _, want := range []string{
		"--upgrade", "BIN.prev", "/etc/rollcall/agent.json",
		"stat -c '%u:%g'", "systemctl show -p ExecStart",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("升级脚本缺少 %q", want)
		}
	}
	// 升级分支必须在安装分支重写配置之前退出，避免覆盖 token/interval/update
	upgradeExit := strings.Index(body, "exit 0\nfi\n\nmv -f \"$TMP\" \"$BIN\"")
	configWrite := strings.Index(body, "cat > /etc/rollcall/agent.json")
	if upgradeExit < 0 || configWrite < 0 || upgradeExit > configWrite {
		t.Fatal("升级分支必须先于配置写入退出")
	}
}

// TestAgentChecksumEndpoint 安装脚本依赖它校验下载内容：
// 必须与二进制逐字节一致，缺失架构时明确 404。
func TestAgentChecksumEndpoint(t *testing.T) {
	dir := t.TempDir()
	content := []byte("#!/bin/sh\nexit 0\n")
	if err := os.WriteFile(filepath.Join(dir, "rollcall-agent-linux-amd64"), content, 0o755); err != nil {
		t.Fatal(err)
	}
	srv := New(store.New(t.TempDir()), nil, nil, Config{AgentDir: dir})

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/agent/download.sha256?arch=amd64", nil))
	if rec.Code != 200 {
		t.Fatalf("校验端点应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	sum := sha256.Sum256(content)
	if got, want := strings.TrimSpace(rec.Body.String()), hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("校验值不符: got %s want %s", got, want)
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/agent/download.sha256?arch=arm64", nil))
	if rec.Code != 404 {
		t.Fatalf("缺失架构应 404，实际 %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/agent/download.sha256?arch=../x", nil))
	if rec.Code != 400 {
		t.Fatalf("非法架构应 400，实际 %d", rec.Code)
	}
}

// TestAgentDownloadServesConfiguredArchs 验证二进制分发：白名单架构、
// 未配置目录时明确报错（而不是给空文件）、支持断点续传头。
func TestAgentDownloadServesConfiguredArchs(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "rollcall-agent-linux-amd64")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	srv := New(store.New(t.TempDir()), nil, nil, Config{AgentDir: dir})

	// 未配置架构（arm64 缺失）→ 404
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/agent/download?arch=arm64", nil))
	if rec.Code != 404 {
		t.Fatalf("缺失架构应 404，实际 %d", rec.Code)
	}

	// 已配置架构 → 200 + 内容
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/agent/download?arch=amd64", nil)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("amd64 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() == "" {
		t.Fatal("下载内容为空")
	}

	// 穿穿越与非法架构必须拒绝
	for _, bad := range []string{"../etc/passwd", "", "win64"} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/agent/download?arch="+bad, nil))
		if rec.Code == 200 {
			t.Fatalf("arch=%q 不应 200", bad)
		}
	}

	// 未配置 AgentDir 的实例 → 404 且说明原因
	srv2 := New(store.New(t.TempDir()), nil, nil, Config{})
	rec = httptest.NewRecorder()
	srv2.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/agent/download?arch=amd64", nil))
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "agent-dir") {
		t.Fatalf("未配置目录应 404 并提示 -agent-dir，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// Command agent 是只读监控代理。
//
// ★ 安全承诺 ★
//   - 不 import os/exec        → 无法执行任意命令
//   - 不监听任何端口            → 无入站攻击面
//   - 不接收服务端指令          → 协议为单向上报
//   - 只读 /proc、/etc/os-release 与 statfs → 不触碰用户数据
//
// 自更新是唯一会修改自身二进制的功能，且必须在本机配置中显式开启：
// 只拉取 Ed25519 签名清单、校验 SHA-256，替换自身后用 syscall.Exec 重启，
// 不经过 shell，也不执行清单以外的任何程序。
//
// 验证方式见 internal/collect/collect_linux.go 顶部注释与 README 第一节。
// 要点：用 `go list -deps ./cmd/agent` 看依赖、用**未剥离**二进制的
// `go tool nm` 看符号，不要 grep 源码（注释本身含这些字样，必然误命中）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"

	"github.com/motao/rollcall/internal/agentwire"
	"strings"
	"sync/atomic"
	"time"

	"github.com/motao/rollcall/internal/collect"
	"github.com/motao/rollcall/internal/model"
	"github.com/motao/rollcall/internal/selfupdate"
	"github.com/motao/rollcall/internal/version"
)

type updateConfig struct {
	Enabled            bool   `json:"enabled"`
	ManifestURL        string `json:"manifestUrl"`
	PublicKey          string `json:"publicKey"`
	CheckIntervalHours int    `json:"checkIntervalHours"` // 1–168，默认 6
}

type config struct {
	Server   string       `json:"server"` // 主控地址，如 https://rollcall.example.com
	NodeID   string       `json:"nodeId"`
	Token    string       `json:"token"`
	Interval int          `json:"interval"` // 秒
	Update   updateConfig `json:"update"`
}

func loadConfig(path string) (*config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	// 上报节奏从 agentwire.OnlineTTL 推出，而不是在 Agent 里再写一份数字：
	// 服务端按那个 TTL 判定离线，两侧各持一份迟早漂移。
	ttl := agentwire.OnlineTTL
	if c.Interval <= 0 {
		c.Interval = agentwire.ReportInterval(ttl)
	}
	// 间隔上限同样由 TTL 推出（不超过 TTL 的一半，保证一个 TTL 内至少两次机会），
	// 下限挡住"每秒上报"这类只会给主控加压的配置。
	clamped, ok := agentwire.ClampReportInterval(c.Interval, ttl)
	if !ok {
		return nil, fmt.Errorf("interval 必须在 %d–%d 秒之间（服务端 %d 秒未收到上报即判定离线），当前 %d 秒",
			agentwire.MinReportInterval, agentwire.MaxReportInterval(ttl), ttl, c.Interval)
	}
	c.Interval = clamped
	if c.Update.Enabled {
		if strings.TrimSpace(c.Update.ManifestURL) == "" || strings.TrimSpace(c.Update.PublicKey) == "" {
			return nil, fmt.Errorf("update.enabled 为 true 时必须配置 manifestUrl 与 publicKey")
		}
		if c.Update.CheckIntervalHours == 0 {
			c.Update.CheckIntervalHours = 6
		}
		if c.Update.CheckIntervalHours < 1 || c.Update.CheckIntervalHours > 168 {
			return nil, fmt.Errorf("update.checkIntervalHours 必须在 1–168 之间")
		}
	}
	return &c, nil
}

func main() {
	cfgPath := flag.String("config", "./agent.json", "配置文件路径")
	once := flag.Bool("once", false, "只采集并上报一次（用于验证连通性）")
	allowInsecure := flag.Bool("allow-insecure", false, "允许用明文 HTTP 连接非本机服务端（节点令牌会明文传输）")
	rollback := flag.Bool("rollback", false, "恢复 .prev 备份的上一版本并重启（手动回滚）")
	updateNow := flag.Bool("update-now", false, "立即检查并安装更新后退出（需在配置中启用 update）")
	showVersion := flag.Bool("version", false, "打印版本后退出")
	flag.Parse()

	if *showVersion {
		fmt.Printf("rollcall-agent %s\n", version.Version)
		return
	}
	if runtime.GOOS != "linux" {
		log.Printf("⚠ 当前平台 %s 仅用于 UI 调试，采集指标将全部为 0；生产请使用 Linux", runtime.GOOS)
	}

	exePath, exeErr := os.Executable()
	if exeErr != nil {
		log.Printf("⚠ 无法定位可执行文件，自更新与回滚不可用: %v", exeErr)
		exePath = ""
	}
	if runtime.GOOS == "linux" && exePath != "" {
		if *rollback {
			if err := selfupdate.ManualRollback(exePath, version.Version); err != nil {
				log.Fatalf("回滚失败: %v", err)
			}
			log.Printf("已恢复上一版本，重新启动…")
			if err := reexec(exePath, stripFlag(os.Args, "-rollback")); err != nil {
				log.Fatalf("重启失败: %v", err)
			}
			return
		}
		rolledBack, err := selfupdate.HandleStartup(exePath, version.Version)
		if err != nil {
			log.Printf("检查更新启动标记失败: %v", err)
		}
		if rolledBack {
			log.Printf("新版本连续 %d 次启动未稳定，已回滚到上一版本", selfupdate.MaxStartupAttempts)
			if err := reexec(exePath, os.Args); err != nil {
				log.Fatalf("重启失败: %v", err)
			}
			return
		}
	} else if *rollback {
		log.Fatal("-rollback 仅支持 Linux")
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("读取配置失败: %v", err)
	}
	if cfg.Server == "" || cfg.NodeID == "" || cfg.Token == "" {
		log.Fatal("配置缺少 server / nodeId / token")
	}

	endpoint, err := reportEndpoint(cfg.Server, *allowInsecure)
	if err != nil {
		log.Fatalf("配置错误: %v", err)
	}

	var updater *selfupdate.Updater
	if cfg.Update.Enabled {
		if runtime.GOOS != "linux" || exePath == "" {
			log.Fatal("自更新仅支持 Linux 且需要可定位的可执行文件")
		}
		updater, err = selfupdate.New(cfg.Update.ManifestURL, cfg.Update.PublicKey, *allowInsecure)
		if err != nil {
			log.Fatalf("更新配置无效: %v", err)
		}
	}
	checkAndApply := func(ctx context.Context) (bool, error) {
		m, err := updater.Fetch(ctx)
		if err != nil {
			return false, err
		}
		key := updater.GOOS + "-" + updater.GOARCH
		if _, ok := m.Assets[key]; !ok {
			if selfupdate.IsNewer(m.Version, version.Version) {
				log.Printf("更新清单 %s 没有适用于 %s 的资产", m.Version, key)
			}
			return false, nil
		}
		asset, newer := updater.Candidate(m, version.Version)
		if !newer {
			return false, nil
		}
		if failed, ok := selfupdate.BlockedVersion(exePath); ok && selfupdate.SameVersion(failed, m.Version) {
			log.Printf("版本 %s 此前启动失败已回滚，%s 内不再自动安装", m.Version, selfupdate.FailedVersionBackoff)
			return false, nil
		}
		path, err := updater.Download(ctx, asset, filepath.Dir(exePath))
		if err != nil {
			return false, err
		}
		if err := selfupdate.Apply(exePath, path, m.Version); err != nil {
			_ = os.Remove(path)
			return false, err
		}
		log.Printf("已安装 Agent %s，上一版本备份为 %s", m.Version, selfupdate.BackupPath(exePath))
		return true, nil
	}

	if *updateNow {
		if updater == nil {
			log.Fatal("-update-now 需要在配置中启用 update")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		updated, err := checkAndApply(ctx)
		cancel()
		if err != nil {
			log.Fatalf("更新检查失败: %v", err)
		}
		if updated {
			return
		}
		log.Printf("未安装更新（已是最新版本，或该版本处于回滚冷却期）")
		return
	}

	c := collect.New()
	host := c.Host()
	log.Printf("节点 %s（%s / %s，%d 核）→ %s", cfg.NodeID, host.Hostname, host.OS, host.Cores, cfg.Server)

	// 首次采集用于建立 CPU 基线；-once 不应让验证脚本干等一个完整上报周期
	c.Collect()
	if *once {
		time.Sleep(1 * time.Second)
	} else {
		time.Sleep(time.Duration(cfg.Interval) * time.Second)
	}

	// 上报接口不需要重定向：拒绝跟随可确保 X-Node-Token 绝不会被转发到其他主机
	client := &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	send := func() error {
		m := c.Collect()
		rep := model.Report{NodeID: cfg.NodeID, Host: host, Metrics: m}
		body, err := json.Marshal(rep)
		if err != nil {
			return fmt.Errorf("序列化失败: %w", err)
		}
		req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("构造请求失败: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Node-Token", cfg.Token)
		// 用请求头而不是 JSON 字段：旧服务端启用了 DisallowUnknownFields，
		// 加字段会让先升级 Agent 的节点整份上报被拒。
		req.Header.Set("X-Agent-Version", version.Version)

		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("上报失败: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("上报被拒绝: HTTP %d", resp.StatusCode)
		}
		log.Printf("已上报 cpu=%.1f%% mem=%.1f%% disk=%.1f%%",
			m.CPUPct, pct(m.MemUsed, m.MemTotal), pct(m.DiskUsed, m.DiskTotal))
		if exePath != "" {
			if err := selfupdate.MarkStable(exePath, version.Version); err != nil {
				log.Printf("清除更新启动标记失败: %v", err)
			}
		}
		return nil
	}

	if *once {
		if err := send(); err != nil {
			log.Printf("验证失败: %v", err)
			os.Exit(1)
		}
		return
	}

	if updater != nil {
		interval := time.Duration(cfg.Update.CheckIntervalHours) * time.Hour
		go func() {
			// 错开启动高峰；首次检查失败只记录，不影响上报。
			timer := time.NewTimer(30 * time.Second)
			defer timer.Stop()
			for range timer.C {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
				updated, err := checkAndApply(ctx)
				cancel()
				if err != nil {
					log.Printf("自动更新检查失败: %v", err)
				}
				if updated {
					log.Printf("自更新已安装，重启 Agent…")
					if err := reexec(exePath, os.Args); err != nil {
						log.Fatalf("重启失败: %v", err)
					}
					return
				}
				timer.Reset(interval)
			}
		}()
	}

	// 上报异步执行：慢网络下同步 send 会堵住 ticker，
	// 且 time.Ticker 只缓冲一个 tick，后续 tick 会被静默丢弃。
	// 用单飞标记保证同一时刻最多一个请求，不堆积 goroutine。
	var sending atomic.Bool
	trigger := func() {
		if !sending.CompareAndSwap(false, true) {
			return
		}
		go func() {
			defer sending.Store(false)
			if err := send(); err != nil {
				log.Print(err)
			}
		}()
	}
	t := time.NewTicker(time.Duration(cfg.Interval) * time.Second)
	defer t.Stop()
	trigger()
	for range t.C {
		trigger()
	}
}

// reportEndpoint 校验并归一化服务端地址。
// 非本机服务端默认必须使用 HTTPS，否则节点令牌会以明文穿越网络。
func reportEndpoint(server string, allowInsecure bool) (string, error) {
	raw := strings.TrimSpace(server)
	server = strings.TrimRight(raw, "/")
	u, err := url.Parse(server)
	// 只看 u.Host 不够：`https://:443` 的 Host 非空但主机名为空，
	// 归一化后会得到一个指向本机的高位端口的地址。
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("server 地址无效: %q", redactServer(raw))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("server 仅支持 http/https: %q", redactServer(raw))
	}
	// 只接受「协议 + 主机 + 可选端口」：带路径、查询串、片段或用户信息时，
	// 简单拼接 /api/report 会得到意料之外的地址（甚至把凭据写进 URL）。
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("server 不能包含路径、查询串、片段或用户信息: %q", redactServer(raw))
	}
	if u.Scheme == "http" && !allowInsecure && !isLoopbackHost(u.Hostname()) {
		return "", fmt.Errorf("server 使用明文 HTTP 会泄露节点令牌，请改用 https 或显式加 -allow-insecure: %q", redactServer(raw))
	}
	return server + "/api/report", nil
}

// redactServer 去掉地址里的用户信息，供错误信息与日志使用。
//
// 这个函数的返回值会被 log.Print 打到 stderr、并被运维贴进工单；
// 而 URL 中允许出现 `https://user:password@host` 形式，
// 原样回显等于把凭据写进日志。
func redactServer(raw string) string {
	if u, err := url.Parse(strings.TrimSpace(raw)); err == nil && u.User != nil {
		u.User = url.User("***")
		return u.String()
	}
	// 解析失败时按 `@` 截断，避免把 `user:pass@` 部分带出去。
	if i := strings.LastIndex(raw, "@"); i >= 0 {
		return "***@" + raw[i+1:]
	}
	if len(raw) > 200 {
		return raw[:200] + "…"
	}
	return raw
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// stripFlag 去掉指定开关，避免回滚后 re-exec 再次触发回滚。
//
// Go 的 flag 包把 -name 与 --name 视为同一个开关（也接受 =value 形式），
// 因此必须先归一化前导横线再比较。早期实现只精确匹配 "-rollback"：
// 用 `rollcall-agent --rollback` 启动时该参数不会被剥离，re-exec 后的进程
// 再次执行回滚并再次 re-exec，形成无限回滚 / 重启循环。
func stripFlag(args []string, name string) []string {
	want := strings.TrimLeft(name, "-")
	out := make([]string, 0, len(args))
	for _, a := range args {
		if flagKey(a) == want {
			continue
		}
		out = append(out, a)
	}
	return out
}

// flagKey 归一化参数名：去掉前导横线与 =value 部分；非开关参数返回空串。
func flagKey(arg string) string {
	if !strings.HasPrefix(arg, "-") {
		return ""
	}
	s := strings.TrimLeft(arg, "-")
	if i := strings.IndexByte(s, '='); i >= 0 {
		s = s[:i]
	}
	return s
}

func pct(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) / float64(total) * 100
}

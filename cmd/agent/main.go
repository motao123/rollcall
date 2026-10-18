// Command agent 是只读监控代理。
//
// ★ 安全承诺 ★
//   - 不 import os/exec        → 无法执行任何命令
//   - 不监听任何端口            → 无入站攻击面
//   - 不接收服务端指令          → 协议为单向上报
//   - 只读 /proc、/etc/os-release 与 statfs → 不触碰用户数据
//
// 验证方式见 internal/collect/collect_linux.go 顶部注释与 README 第一节。
// 要点：用 `go list -deps ./cmd/agent` 看依赖、用**未剥离**二进制的
// `go tool nm` 看符号，不要 grep 源码（注释本身含这些字样，必然误命中）。
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/motao/rollcall/internal/collect"
	"github.com/motao/rollcall/internal/model"
)

type config struct {
	Server   string `json:"server"` // 主控地址，如 https://rollcall.example.com
	NodeID   string `json:"nodeId"`
	Token    string `json:"token"`
	Interval int    `json:"interval"` // 秒
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
	if c.Interval <= 0 {
		c.Interval = 30
	}
	// 上限防止 time.Duration 乘法溢出成负数，触发 NewTicker panic
	if c.Interval > 86400 {
		return nil, fmt.Errorf("interval 不能超过 86400 秒")
	}
	return &c, nil
}

func main() {
	cfgPath := flag.String("config", "./agent.json", "配置文件路径")
	once := flag.Bool("once", false, "只采集并上报一次（用于验证连通性）")
	allowInsecure := flag.Bool("allow-insecure", false, "允许用明文 HTTP 连接非本机服务端（节点令牌会明文传输）")
	flag.Parse()

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

	c := collect.New()
	host := c.Host()
	log.Printf("节点 %s（%s / %s，%d 核）→ %s", cfg.NodeID, host.Hostname, host.OS, host.Cores, cfg.Server)

	// 首次采集用于建立 CPU 基线
	c.Collect()
	time.Sleep(time.Duration(cfg.Interval) * time.Second)

	// 上报接口不需要重定向：拒绝跟随可确保 X-Node-Token 绝不会被转发到其他主机
	client := &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	send := func() {
		m := c.Collect()
		rep := model.Report{NodeID: cfg.NodeID, Host: host, Metrics: m}
		body, err := json.Marshal(rep)
		if err != nil {
			log.Printf("序列化失败: %v", err)
			return
		}
		req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			log.Printf("构造请求失败: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Node-Token", cfg.Token)

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("上报失败: %v", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			log.Printf("上报被拒绝: HTTP %d", resp.StatusCode)
			return
		}
		log.Printf("已上报 cpu=%.1f%% mem=%.1f%% disk=%.1f%%",
			m.CPUPct, pct(m.MemUsed, m.MemTotal), pct(m.DiskUsed, m.DiskTotal))
	}

	if *once {
		send()
		return
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
			send()
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
	server = strings.TrimRight(strings.TrimSpace(server), "/")
	u, err := url.Parse(server)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("server 地址无效: %q", server)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("server 仅支持 http/https: %q", server)
	}
	if u.Scheme == "http" && !allowInsecure && !isLoopbackHost(u.Hostname()) {
		return "", fmt.Errorf("server 使用明文 HTTP 会泄露节点令牌，请改用 https 或显式加 -allow-insecure")
	}
	return server + "/api/report", nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func pct(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) / float64(total) * 100
}

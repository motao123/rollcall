// Command agent 是只读监控代理。
//
// ★ 安全承诺 ★
//   - 不 import os/exec        → 无法执行任何命令
//   - 不监听任何端口            → 无入站攻击面
//   - 不接收服务端指令          → 协议为单向上报
//   - 只读 /proc 与 statfs      → 不触碰用户数据
//
// 验证方式见 internal/collect/collect_linux.go 顶部注释与 README 第一节。
// 要点：用 `go list -deps ./cmd/agent` 看依赖、用**未剥离**二进制的
// `go tool nm` 看符号，不要 grep 源码（注释本身含这些字样，必然误命中）。
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
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
	return &c, nil
}

func main() {
	cfgPath := flag.String("config", "./agent.json", "配置文件路径")
	once := flag.Bool("once", false, "只采集并上报一次（用于验证连通性）")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("读取配置失败: %v", err)
	}
	if cfg.Server == "" || cfg.NodeID == "" || cfg.Token == "" {
		log.Fatal("配置缺少 server / nodeId / token")
	}

	c := collect.New()
	host := c.Host()
	log.Printf("节点 %s（%s / %s，%d 核）→ %s", cfg.NodeID, host.Hostname, host.OS, host.Cores, cfg.Server)

	// 首次采集用于建立 CPU 基线
	c.Collect()
	time.Sleep(time.Duration(cfg.Interval) * time.Second)

	client := &http.Client{Timeout: 15 * time.Second}
	endpoint := cfg.Server + "/api/report"

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

	t := time.NewTicker(time.Duration(cfg.Interval) * time.Second)
	defer t.Stop()
	send()
	for range t.C {
		send()
	}
}

func pct(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) / float64(total) * 100
}

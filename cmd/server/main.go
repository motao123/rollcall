// Command server 是监控平台的服务端。
//
// 单二进制 + 内嵌前端 + JSON 快照存储，零外部依赖。
package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/motao/probe/internal/alert"
	"github.com/motao/probe/internal/probe"
	"github.com/motao/probe/internal/store"
	"github.com/motao/probe/internal/web"
)

func main() {
	addr := flag.String("addr", ":9080", "监听地址")
	dataDir := flag.String("data", "./data", "数据目录")
	demo := flag.Bool("demo", false, "载入演示数据（仅用于预览 UI）")
	probeEvery := flag.Duration("probe-interval", 30*time.Second, "服务探测间隔")
	adminToken := flag.String("admin-token", "", "管理 API 令牌（留空则读取/生成 data/admin.token）")
	auth := flag.String("auth", "", "Basic Auth，格式 user:password（留空则不启用）")
	panelLink := flag.String("panel-link", "", "面板对外地址（拼进告警通知，可留空）")
	flag.Parse()

	st := store.New(*dataDir)
	log.Printf("存储目录: %s", *dataDir)

	// 管理令牌：优先使用显式传入的，否则从数据目录读取或生成
	adm := *adminToken
	if adm == "" {
		adm = loadOrCreateAdminToken(*dataDir)
	}
	if adm != "" {
		log.Printf("管理 API 已启用（X-Admin-Token，令牌长度 %d）", len(adm))
	}

	// Basic Auth
	var authUser, authPass string
	if *auth != "" {
		parts := strings.SplitN(*auth, ":", 2)
		if len(parts) != 2 || parts[0] == "" {
			log.Fatal("-auth 格式应为 user:password")
		}
		authUser, authPass = parts[0], parts[1]
		log.Printf("Basic Auth 已启用（用户 %s）", authUser)
	} else {
		log.Printf("⚠ 未启用 Basic Auth，建议对外暴露时通过面板反代加访问控制")
	}

	if *demo {
		seedDemo(st)
		log.Printf("演示数据已载入")
	}

	// 默认告警规则（仅当没有任何规则时）
	if len(st.Rules()) == 0 {
		for _, r := range alert.DefaultRules() {
			st.UpsertRule(r)
		}
	}

	runner := probe.NewRunner()
	engine := alert.New(st)
	engine.Start(15 * time.Second)
	st.StartAutoSave(60 * time.Second)

	// 服务探测循环
	// 注意：演示模式下不发起真实探测，避免对本机产生意外外连
	if !*demo {
		go func() {
			t := time.NewTicker(*probeEvery)
			defer t.Stop()
			for {
				for _, sv := range st.Services() {
					if !sv.Enabled {
						continue
					}
					res := runner.Do(*sv)
					st.AddServiceResult(res)
				}
				<-t.C
			}
		}()
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           web.New(st, runner, engine, web.Config{AdminToken: adm, AuthUser: authUser, AuthPass: authPass, PanelLink: *panelLink}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("监控平台已启动 → http://%s", displayAddr(*addr))
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

// loadOrCreateAdminToken 从数据目录读取管理令牌，不存在则生成并落盘（权限 0600）。
func loadOrCreateAdminToken(dir string) string {
	p := filepath.Join(dir, "admin.token")
	if b, err := os.ReadFile(p); err == nil {
		if v := strings.TrimSpace(string(b)); v != "" {
			return v
		}
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		log.Fatalf("生成管理令牌失败: %v", err)
	}
	tok := hex.EncodeToString(buf)
	if err := os.WriteFile(p, []byte(tok+"\n"), 0o600); err != nil {
		log.Printf("⚠ 管理令牌无法写入 %s: %v（仅本次进程有效）", p, err)
		return tok
	}
	log.Printf("已生成管理令牌 → %s", p)
	return tok
}

// displayAddr 把 ":9188" / "127.0.0.1:9188" 归一化为可点击的地址。
func displayAddr(a string) string {
	if a == "" {
		return "localhost:9080"
	}
	if a[0] == ':' {
		return "localhost" + a
	}
	return a
}

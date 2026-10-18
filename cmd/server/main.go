// Command server 是监控平台的服务端。
//
// 单二进制 + 内嵌前端 + JSON 快照存储，零外部依赖。
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/motao/rollcall/internal/alert"
	"github.com/motao/rollcall/internal/notify"
	"github.com/motao/rollcall/internal/probe"
	"github.com/motao/rollcall/internal/store"
	"github.com/motao/rollcall/internal/web"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:19080", "监听地址（默认仅回环）")
	dataDir := flag.String("data", "./data", "数据目录")
	demo := flag.Bool("demo", false, "载入演示数据（仅用于预览 UI）")
	probeEvery := flag.Duration("probe-interval", 10*time.Second, "探测调度粒度（每个服务仍按自身 interval 执行）")
	adminToken := flag.String("admin-token", "", "管理 API 令牌（留空则读取/生成 data/admin.token）")
	auth := flag.String("auth", "", "Basic Auth，格式 user:password（留空则不启用）")
	panelLink := flag.String("panel-link", "", "面板对外地址（拼进告警通知，可留空）")
	tlsCert := flag.String("tls-cert", "", "TLS 证书路径（与 -tls-key 同时提供则启用 HTTPS）")
	tlsKey := flag.String("tls-key", "", "TLS 私钥路径")
	allowPublic := flag.Bool("allow-public", false, "允许未启用 Basic Auth 时监听非回环地址（仪表盘数据仍需管理令牌，/status 保持公开）")
	allowInsecure := flag.Bool("allow-insecure", false, "允许在非回环地址上使用明文 HTTP（不建议）")
	allowPrivateProbes := flag.Bool("allow-private-probes", false, "允许探测回环/私网/链路本地目标")
	insecureProbeTLS := flag.Bool("insecure-probe-tls", false, "探测 HTTPS 时跳过证书校验（仅用于自签名目标）")
	allowPrivateNotify := flag.Bool("allow-private-notify", false, "允许通知通道访问回环/私网目标（自建 Gotify/Bark/Webhook 时使用）")
	flag.Parse()

	if *probeEvery <= 0 {
		log.Fatal("-probe-interval 必须大于 0")
	}
	if *probeEvery > 24*time.Hour {
		log.Fatal("-probe-interval 不能超过 24h")
	}
	if (*tlsCert == "") != (*tlsKey == "") {
		log.Fatal("-tls-cert 与 -tls-key 必须同时提供")
	}

	// 演示模式使用临时数据目录：预览数据不应污染真实台账与令牌文件。
	if *demo {
		dir, err := os.MkdirTemp("", "rollcall-demo-*")
		if err != nil {
			log.Fatalf("创建演示数据目录失败: %v", err)
		}
		defer os.RemoveAll(dir)
		*dataDir = dir
		log.Printf("演示模式：使用临时数据目录 %s（退出后自动删除）", dir)
	}

	st := store.New(*dataDir)
	if err := st.LoadErr(); err != nil {
		log.Fatalf("载入持久化状态失败: %v", err)
	}
	log.Printf("存储目录: %s", *dataDir)

	// 管理令牌：优先使用显式传入的，否则从数据目录读取或生成
	adm := *adminToken
	if adm == "" {
		var err error
		adm, err = loadOrCreateAdminToken(*dataDir)
		if err != nil {
			log.Fatalf("管理令牌不可用: %v", err)
		}
	}
	if *adminToken != "" && len(*adminToken) < 16 {
		log.Fatal("-admin-token 至少需要 16 个字符；留空可自动生成 192 位令牌")
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
		if parts[1] == "" {
			log.Fatal("-auth 密码不能为空")
		}
		authUser, authPass = parts[0], parts[1]
		if len(authPass) < 12 {
			log.Printf("⚠ Basic Auth 密码较短（%d 字符），建议至少 12 个字符", len(authPass))
		}
		log.Printf("Basic Auth 已启用（用户 %s）", authUser)
	}

	// 安全默认：非回环绑定必须显式确认暴露面。
	loopback := isLoopbackListen(*addr)
	if !loopback {
		if authUser == "" && !*allowPublic {
			log.Fatal("拒绝在未启用 Basic Auth 时监听非回环地址；请加 -auth，或显式加 -allow-public（仪表盘数据仍需管理令牌，/status 保持公开）")
		}
		if *tlsCert == "" && !*allowInsecure {
			log.Fatal("拒绝在非回环地址上使用明文 HTTP；请配置 -tls-cert/-tls-key，或通过反向代理/隧道，或显式加 -allow-insecure")
		}
	} else if authUser == "" {
		log.Printf("⚠ 未启用 Basic Auth；当前仅监听回环，请通过反向代理/隧道访问")
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

	runner := probe.NewRunner(probe.Config{
		AllowPrivate: *allowPrivateProbes,
		InsecureTLS:  *insecureProbeTLS,
	})
	notify.SetAllowPrivateTargets(*allowPrivateNotify)
	engine := alert.New(st)
	engine.Start(15 * time.Second)
	st.StartAutoSave(60 * time.Second)

	// 服务探测循环
	// 注意：演示模式下不发起真实探测，避免对本机产生意外外连
	if !*demo {
		go func() {
			t := time.NewTicker(*probeEvery)
			defer t.Stop()
			last := map[string]time.Time{}
			for {
				now := time.Now()
				for _, sv := range st.Services() {
					if !sv.Enabled {
						continue
					}
					interval := time.Duration(sv.Interval) * time.Second
					if interval <= 0 {
						interval = 60 * time.Second
					}
					if t, ok := last[sv.ID]; ok && now.Sub(t) < interval {
						continue
					}
					last[sv.ID] = now
					res := runner.Do(*sv)
					st.AddServiceResult(res)
				}
				// 清理已删除服务的时间戳，避免 map 随增删持续增长
				if len(last) > 2*len(st.Services())+32 {
					for id := range last {
						found := false
						for _, sv := range st.Services() {
							if sv.ID == id {
								found = true
								break
							}
						}
						if !found {
							delete(last, id)
						}
					}
				}
				<-t.C
			}
		}()
	}

	srv := &http.Server{
		Addr:              *addr,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	ws := web.New(st, runner, engine, web.Config{
		AdminToken: adm, AuthUser: authUser, AuthPass: authPass, PanelLink: *panelLink,
	})
	if err := ws.NotifyErr(); err != nil {
		log.Printf("⚠ 已保存的通知配置无法解析，通知已停用: %v", err)
	}
	srv.Handler = ws.Handler()

	scheme := "http"
	if *tlsCert != "" {
		scheme = "https"
	}
	log.Printf("监控平台已启动 → %s://%s", scheme, displayAddr(*addr))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		if *tlsCert != "" {
			errCh <- srv.ListenAndServeTLS(*tlsCert, *tlsKey)
			return
		}
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	case <-ctx.Done():
		log.Printf("收到退出信号，正在关闭并保存状态…")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			log.Printf("HTTP 服务关闭超时: %v", err)
		}
		if err := st.Save(); err != nil {
			log.Printf("退出前保存失败: %v", err)
		} else {
			log.Printf("状态已保存，退出")
		}
	}
}

// isLoopbackListen 判断监听地址是否只绑定回环，包含 ":9080" 这种全接口写法。
func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// loadOrCreateAdminToken 从数据目录读取管理令牌，不存在则生成并落盘（权限 0600）。
// 读取失败（权限、IO 错误、文件为空）必须报错而不是悄悄换新令牌 ——
// 那会让所有已保存令牌的面板与脚本立即失效，且原因难以排查。
func loadOrCreateAdminToken(dir string) (string, error) {
	p := filepath.Join(dir, "admin.token")
	b, err := os.ReadFile(p)
	if err == nil {
		v := strings.TrimSpace(string(b))
		if v == "" {
			return "", fmt.Errorf("令牌文件 %s 为空，请删除后重启以重新生成", p)
		}
		// 历史版本或手工拷贝可能留下 0644：收紧到 0600 再使用。
		if err := os.Chmod(p, 0o600); err != nil {
			return "", fmt.Errorf("收紧 %s 权限为 0600: %w", p, err)
		}
		return v, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("读取 %s: %w", p, err)
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成管理令牌: %w", err)
	}
	tok := hex.EncodeToString(buf)
	if err := os.WriteFile(p, []byte(tok+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("写入 %s: %w", p, err)
	}
	log.Printf("已生成管理令牌 → %s", p)
	return tok, nil
}

// displayAddr 把 ":9188" / "127.0.0.1:9188" 归一化为可点击的地址。
func displayAddr(a string) string {
	if a == "" {
		return "127.0.0.1:9080"
	}
	if a[0] == ':' {
		return "localhost" + a
	}
	return a
}

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
	"sync"
	"syscall"
	"time"

	"github.com/motao123/rollcall/internal/alert"
	"github.com/motao123/rollcall/internal/auth"
	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/notify"
	"github.com/motao123/rollcall/internal/outbox"
	"github.com/motao123/rollcall/internal/probe"
	"github.com/motao123/rollcall/internal/schedule"
	"github.com/motao123/rollcall/internal/store"
	"github.com/motao123/rollcall/internal/version"
	"github.com/motao123/rollcall/internal/web"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:19080", "监听地址（默认仅回环）")
	dataDir := flag.String("data", "./data", "数据目录")
	demo := flag.Bool("demo", false, "载入演示数据（仅用于预览 UI）")
	probeEvery := flag.Duration("probe-interval", 10*time.Second, "探测调度粒度（每个服务仍按自身 interval 执行）")
	adminToken := flag.String("admin-token", "", "管理 API 令牌（留空则读取/生成 data/admin.token）")
	adminUser := flag.String("admin-user", "", "面板管理员用户名（留空读 ROLLCALL_ADMIN_USER，默认 admin）")
	adminEmail := flag.String("admin-email", "", "面板管理员邮箱（可选；留空读 ROLLCALL_ADMIN_EMAIL）")
	adminPassword := flag.String("admin-password", "", "初始/重置密码，至少 12 位（建议用 ROLLCALL_ADMIN_PASSWORD；仅在首次创建或 -reset-admin-password 时生效）")
	resetAdminPassword := flag.Bool("reset-admin-password", false, "用 -admin-password/ROLLCALL_ADMIN_PASSWORD 重置管理员密码，并使旧会话失效")
	basicAuth := flag.String("auth", "", "兼容用 Basic Auth，格式 user:password（面板 UI 不再使用，留空则不启用）")
	panelLink := flag.String("panel-link", "", "面板对外地址（拼进告警通知，可留空）")
	tlsCert := flag.String("tls-cert", "", "TLS 证书路径（与 -tls-key 同时提供则启用 HTTPS）")
	tlsKey := flag.String("tls-key", "", "TLS 私钥路径")
	allowPublic := flag.Bool("allow-public", false, "已废弃：面板账号登录默认启用，该开关不再影响启动")
	allowInsecure := flag.Bool("allow-insecure", false, "允许在非回环地址上使用明文 HTTP（不建议）")
	allowPrivateProbes := flag.Bool("allow-private-probes", false, "允许探测回环/私网/链路本地目标")
	insecureProbeTLS := flag.Bool("insecure-probe-tls", false, "探测 HTTPS 时跳过证书校验（仅用于自签名目标）")
	allowPrivateNotify := flag.Bool("allow-private-notify", false, "允许通知通道访问回环/私网目标（自建 Gotify/Bark/Webhook 时使用）")
	agentDir := flag.String("agent-dir", "", "Agent 二进制分发目录（放置 rollcall-agent-linux-<arch>，启用「一条命令接入」）")
	notifyPlugins := flag.String("notify-plugins", "", "通知插件目录（显式启用本地插件，目录内 *.json 清单）")
	trustedProxies := flag.String("trusted-proxies", "",
		"可信反向代理网段（逗号分隔 CIDR，如 127.0.0.1/32,10.0.0.0/8）；留空仅信任回环")
	saveInterval := flag.Duration("save-interval", 60*time.Second, "自动保存快照间隔（10s–1h）")
	history1m := flag.Duration("history-1m-retention", 7*24*time.Hour, "1 分钟聚合层保留时长（默认 7 天）")
	history5m := flag.Duration("history-5m-retention", 30*24*time.Hour, "5 分钟聚合层保留时长（默认 30 天）")
	history1h := flag.Duration("history-1h-retention", 365*24*time.Hour, "1 小时聚合层保留时长（默认 365 天）")
	cnyRate := flag.Float64("cny-rate", 7.2, "CNY 折算 USD 的汇率（用于成本聚合）")
	showVersion := flag.Bool("version", false, "打印版本后退出")
	flag.Parse()

	if *showVersion {
		fmt.Printf("rollcall-server %s\n", version.Version)
		return
	}
	if *probeEvery <= 0 {
		log.Fatal("-probe-interval 必须大于 0")
	}
	if *probeEvery > 24*time.Hour {
		log.Fatal("-probe-interval 不能超过 24h")
	}
	if *saveInterval < 10*time.Second || *saveInterval > time.Hour {
		log.Fatal("-save-interval 必须在 10s–1h 之间")
	}
	if *cnyRate <= 0 || *cnyRate > 1000 {
		log.Fatal("-cny-rate 必须在 0–1000 之间")
	}
	retention := store.HistoryRetention{Tier1m: *history1m, Tier5m: *history5m, Tier1h: *history1h}
	if err := store.ConfigureHistory(retention); err != nil {
		log.Fatalf("历史保留期配置无效: %v", err)
	}
	log.Printf("历史聚合保留: 1m=%s 5m=%s 1h=%s", retention.Tier1m, retention.Tier5m, retention.Tier1h)
	if (*tlsCert == "") != (*tlsKey == "") {
		log.Fatal("-tls-cert 与 -tls-key 必须同时提供")
	}

	// Basic Auth
	var authUser, authPass string
	if *basicAuth != "" {
		parts := strings.SplitN(*basicAuth, ":", 2)
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
	// 面板始终要求账号密码登录，因此不再要求额外配置 Basic Auth；
	// 但明文 HTTP 会把密码暴露在链路上，非回环仍必须有 TLS 或显式确认。
	//
	// ★ 必须在创建任何凭据文件之前完成：被拒绝的启动不应在数据目录里
	// 留下 admin-user.json / admin.token / session.key，否则用户修正参数
	// 后仍会复用这些文件（随机初始密码也已写进 initial-admin-password.txt）。
	loopback := isLoopbackListen(*addr)
	if !loopback {
		if *tlsCert == "" && !*allowInsecure {
			log.Fatal("拒绝在非回环地址上使用明文 HTTP；请配置 -tls-cert/-tls-key，或通过反向代理/隧道，或显式加 -allow-insecure")
		}
	} else if authUser == "" {
		log.Printf("⚠ 当前仅监听回环，请通过反向代理/隧道访问")
	}
	if *allowPublic {
		log.Printf("提示: -allow-public 已废弃；面板账号登录始终启用，该开关不再影响启动")
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

	// 面板管理员账号：用户名/邮箱来自配置，密码仅以 PBKDF2 哈希落盘。
	// 放在存储加载之前：即使快照损坏，也仍能通过 -reset-admin-password 救回面板。
	loginUser := *adminUser
	if loginUser == "" {
		loginUser = os.Getenv("ROLLCALL_ADMIN_USER")
	}
	loginEmail := *adminEmail
	if loginEmail == "" {
		loginEmail = os.Getenv("ROLLCALL_ADMIN_EMAIL")
	}
	loginPassword := *adminPassword
	if loginPassword == "" {
		loginPassword = os.Getenv("ROLLCALL_ADMIN_PASSWORD")
	}
	userPath := filepath.Join(*dataDir, "admin-user.json")
	panelUser, userCreated, generatedPwd, err := auth.LoadOrCreateUser(
		userPath,
		loginUser, loginEmail, loginPassword, *resetAdminPassword)
	if err != nil {
		log.Fatalf("管理员账号配置失败: %v", err)
	}
	if *resetAdminPassword {
		log.Printf("管理员密码已重置: %s（重启服务后生效，旧会话将全部失效）", panelUser.Username)
		return
	}
	switch {
	case userCreated:
		if panelUser.Email != "" {
			log.Printf("面板管理员已创建: %s（可用邮箱 %s 登录）", panelUser.Username, panelUser.Email)
		} else {
			log.Printf("面板管理员已创建: %s", panelUser.Username)
		}
	}
	if generatedPwd != "" {
		announceGeneratedPassword(*dataDir, generatedPwd)
	}
	log.Printf("面板账号登录已启用（用户 %s，会话有效期 %s）", panelUser.Username, auth.DefaultSessionTTL)

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
	if *adminToken != "" && len(*adminToken) < minAdminTokenLen {
		log.Fatalf("-admin-token 至少需要 %d 个字符；留空可自动生成 192 位令牌", minAdminTokenLen)
	}
	if adm != "" {
		log.Printf("管理 API 已启用（X-Admin-Token，令牌长度 %d）", len(adm))
	}

	sessionKey, err := auth.LoadOrCreateSessionKey(filepath.Join(*dataDir, "session.key"))
	if err != nil {
		log.Fatalf("会话密钥不可用: %v", err)
	}
	sessions, err := auth.NewSessions(sessionKey, auth.DefaultSessionTTL)
	if err != nil {
		log.Fatalf("会话管理器初始化失败: %v", err)
	}

	if *demo {
		seedDemo(st)
		log.Printf("演示数据已载入")
	}

	ensureDefaultRules(st)

	runner := probe.NewRunner(probe.Config{
		AllowPrivate: *allowPrivateProbes,
		InsecureTLS:  *insecureProbeTLS,
	})
	notify.SetAllowPrivateTargets(*allowPrivateNotify)
	if err := notify.LoadPlugins(*notifyPlugins); err != nil {
		log.Fatalf("加载通知插件失败: %v", err)
	}
	if n := notify.PluginCount(); n > 0 {
		log.Printf("已加载 %d 个通知插件（目录 %s）", n, *notifyPlugins)
	}
	engine := alert.New(st)
	engine.Start(15 * time.Second)
	st.StartAutoSave(*saveInterval)
	st.StartHistoryAutoSave(store.HistorySaveInterval)
	log.Printf("自动保存间隔: %s（历史聚合 %s）", *saveInterval, store.HistorySaveInterval)

	// bgStop 用于关停时先停掉生产侧（探测循环）：
	// 否则最终快照之后仍会有 AddServiceResult 写入，那些结果永远不会落盘，
	// 而且关机期间还在对外发起探测。
	bgStop := make(chan struct{})
	probeDone := make(chan struct{})

	// 服务探测循环
	// 注意：演示模式下不发起真实探测，避免对本机产生意外外连
	if !*demo {
		go func() {
			defer close(probeDone)
			t := time.NewTicker(*probeEvery)
			defer t.Stop()
			last := map[string]time.Time{}
			for {
				// 先看停止信号：刚启动就关机时不必等满一个 tick。
				select {
				case <-bgStop:
					return
				default:
				}
				svcs := st.Services()
				due := make([]*model.Service, 0, len(svcs))
				for _, sv := range svcs {
					if !sv.Enabled {
						continue
					}
					interval := time.Duration(sv.Interval) * time.Second
					if interval <= 0 {
						interval = 60 * time.Second
					}
					// 用「此刻」而不是本轮起点比较：ticker 略微提前触发时，
					// 按起点算会把间隔判短，实际探测周期被拉长一倍。
					if t0, ok := last[sv.ID]; ok && time.Since(t0) < interval {
						continue
					}
					last[sv.ID] = time.Now()
					due = append(due, sv)
				}
				// 并发探测：单个黑洞目标不应阻塞其余所有服务。
				// 上限 8 与通知队列的默认 worker 数一致。
				var wg sync.WaitGroup
				sem := make(chan struct{}, 8)
				for _, sv := range due {
					wg.Add(1)
					go func(sv *model.Service) {
						defer wg.Done()
						sem <- struct{}{}
						defer func() { <-sem }()
						st.AddServiceResult(runner.Do(*sv))
					}(sv)
				}
				wg.Wait()
				// 清理已删除服务的时间戳，避免 map 随增删持续增长。
				// Services() 只取一次，原实现每个过期 ID 都重新复制+排序一遍。
				if len(last) > 2*len(svcs)+32 {
					alive := make(map[string]bool, len(svcs))
					for _, sv := range svcs {
						alive[sv.ID] = true
					}
					for id := range last {
						if !alive[id] {
							delete(last, id)
						}
					}
				}
				select {
				case <-t.C:
				case <-bgStop:
					return
				}
			}
		}()
	} else {
		close(probeDone)
	}

	stopProducers := func() {
		select {
		case <-bgStop:
		default:
			close(bgStop)
		}
		// 等待探测循环真正退出：它只在每轮结束时检查 bgStop，
		// 不 join 的话关机保存之后仍可能有 AddServiceResult 写入并丢失。
		// 黑洞目标最坏要等单轮超时，这里给 15 秒上限，避免拖死整个关机。
		select {
		case <-probeDone:
		case <-time.After(15 * time.Second):
			log.Printf("⚠ 探测循环 15 秒内未退出，继续关闭流程")
		}
	}

	srv := &http.Server{
		Addr:              *addr,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	ob := outbox.New(st, outbox.DefaultConfig())
	trustedNets, err := web.ParseTrustedProxies(*trustedProxies)
	if err != nil {
		log.Fatalf("-trusted-proxies 无效: %v", err)
	}
	ws := web.New(st, runner, engine, web.Config{
		AdminToken: adm, AuthUser: authUser, AuthPass: authPass,
		User: panelUser, UserPath: userPath, Sessions: sessions, PanelLink: *panelLink,
		CNYRate: *cnyRate, Outbox: ob, TrustedProxies: trustedNets, AgentDir: *agentDir,
		DataDir: *dataDir, SaveInterval: *saveInterval,
	})
	if err := ws.NotifyErr(); err != nil {
		log.Printf("⚠ 已保存的通知配置无法解析，通知已停用: %v", err)
	}
	ob.Start()
	defer ob.Stop()
	sched := schedule.New(st, ob, schedule.Config{PanelLink: *panelLink, CNYRate: *cnyRate})
	sched.Start()
	defer sched.Stop()
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
			// 不能直接 log.Fatal：它走 os.Exit(1)，会跳过 main 里所有 defer
			// （ob.Stop），退出前的最终落盘也永远不会执行 —— 丢掉的正是最近
			// 一个快照周期内的状态（待投递通知、探测结果）。
			if serr := shutdown(srv, ob, st, stopProducers, sched.Stop, "HTTP 服务异常"); serr != nil {
				log.Printf("退出前保存失败: %v", serr)
			}
			log.Fatalf("%v", err)
		}
	case <-ctx.Done():
		// 最终快照写失败时以非零状态退出：调用方（systemd/容器）
		// 需要能区分「干净关机」与「关机时丢数据」。
		if serr := shutdown(srv, ob, st, stopProducers, sched.Stop, "收到退出信号"); serr != nil {
			log.Fatalf("退出前保存失败: %v", serr)
		}
	}
}

// shutdown 关闭 HTTP 服务、排空通知队列并落盘。
// 正常退出与异常退出共用同一路径，避免后者丢掉最后一个快照周期。
//
// 顺序有意为之：
//  1. 先停生产侧（探测循环），否则保存之后仍有结果写入并永久丢失；
//  2. Shutdown 超时则 Close 强制断开剩余连接 —— 否则那些 handler 会被
//     进程退出直接掐掉，正在提交的管理变更写一半；
//  3. 排空通知队列；4. 最后落盘，让哨兵记录覆盖在途投递的最终状态。
func shutdown(srv *http.Server, ob *outbox.Manager, st *store.Store, stopProducers, stopScheduler func(), reason string) error {
	started := time.Now()
	log.Printf("%s，正在关闭并保存状态…", reason)
	if stopProducers != nil {
		stopProducers()
	}
	// 定时任务会入队通知，必须在排空队列之前停掉。
	if stopScheduler != nil {
		stopScheduler()
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Printf("HTTP 服务关闭超时，强制断开剩余连接: %v", err)
		_ = srv.Close()
	}
	ob.Stop()
	saveErr := st.Save()
	if err := st.SaveHistory(); err != nil {
		log.Printf("退出前保存历史失败: %v", err)
	}
	if saveErr != nil {
		log.Printf("退出前保存失败: %v", saveErr)
		return saveErr
	}
	log.Printf("状态已保存，退出（关停耗时 %.1fs）", time.Since(started).Seconds())
	return nil
}

// ensureDefaultRules 只在从未初始化过的实例上播种默认规则。
// 用持久化标记区分「全新实例」与「用户主动删光了所有规则」：
// 后者重启时不应把规则复活。
func ensureDefaultRules(st *store.Store) {
	const key = "rulesInitialized"
	if st.GetSetting(key) != "" {
		return
	}
	// 标记缺失意味着「播种从未完整跑完」。此时补齐缺失的默认规则即可：
	// 如果上次在两次 Upsert 之间崩溃，只按 len(Rules())==0 判断会把
	// 半套规则当成用户主动删光，永久固化残缺状态。
	have := make(map[string]bool)
	for _, r := range st.Rules() {
		have[r.ID] = true
	}
	for _, r := range alert.DefaultRules() {
		if !have[r.ID] {
			st.UpsertRule(r)
		}
	}
	st.SetSetting(key, "1")
	// 规则与标记必须在同一次快照里落盘：自动保存尚未启动，
	// 这里显式保存一次，避免崩溃后出现「有标记但没规则」。
	if err := st.Save(); err != nil {
		log.Printf("保存默认规则失败（将随自动保存重试）: %v", err)
	}
}

// isLoopbackListen 判断监听地址是否只绑定回环，包含 ":19080" 这种全接口写法。
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

// minAdminTokenLen 是管理令牌的最小长度（与自动生成的 48 位十六进制对齐取整）。
const minAdminTokenLen = 16

// announceGeneratedPassword 只在交互式终端打印初始密码。
// systemd 下 stdout 会进 journal，等于把唯一的管理员凭据写进日志与
// 日志采集系统；非 TTY 时改为写入 0600 文件，只记录路径。
func announceGeneratedPassword(dataDir, pwd string) {
	if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		fmt.Printf("初始管理员密码: %s（仅本次启动打印一次，请立即保存；落盘仅保存哈希）\n", pwd)
		return
	}
	path := filepath.Join(dataDir, auth.InitialPasswordFile)
	if err := os.WriteFile(path, []byte(pwd+"\n"), 0o600); err != nil {
		log.Printf("无法安全输出初始管理员密码（写入 %s 失败: %v）；请使用 -reset-admin-password 重新设置", path, err)
		return
	}
	_ = os.Chmod(path, 0o600)
	log.Printf("初始管理员密码已写入 %s（仅本次启动生成；首次登录成功后自动删除）", path)
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
		// 与 -admin-token 同一强度策略。只校验命令行参数时，
		// 把文件截断/换成一个短字符串就能绕过「至少 16 字符」的要求。
		if len(v) < minAdminTokenLen {
			return "", fmt.Errorf("令牌文件 %s 只有 %d 个字符，不足 %d；请删除该文件后重启以重新生成，或恢复完整令牌",
				p, len(v), minAdminTokenLen)
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

// displayAddr 把 ":19080" / "127.0.0.1:19080" 归一化为可点击的地址。
func displayAddr(a string) string {
	if a == "" {
		return "127.0.0.1:19080"
	}
	if a[0] == ':' {
		return "localhost" + a
	}
	return a
}

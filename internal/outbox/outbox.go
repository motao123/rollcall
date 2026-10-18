// Package outbox 实现通知的持久化投递队列。
//
// 告警引擎只负责把消息写入队列，投递由固定数量的 worker 异步完成：
//   - 队列落盘，进程重启后未完成任务继续重试
//   - 并发有上限，告警风暴不会无限起 goroutine
//   - 失败按指数退避重试，耗尽后保留为 failed 供指标与日志排查
//   - 静默期内的任务标记 suppressed，不实际外发
package outbox

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/motao/rollcall/internal/model"
	"github.com/motao/rollcall/internal/notify"
	"github.com/motao/rollcall/internal/store"
)

// Config 是投递队列的运行参数。
type Config struct {
	Workers     int           // 同时投递的任务数
	Tick        time.Duration // 调度轮询间隔
	MaxAttempts int           // 单任务最大投递次数（含首投：5 表示首投 + 4 次重试）
	Backoff     []time.Duration
	// DrainTimeout 是 Stop 等待在途投递完成的上限，0 表示按 Workers 推导。
	// 固定 20 秒不够：notify.Send 串行遍历通道，单个 HTTP 通道就有 12 秒超时，
	// 超时后 Stop 返回但 worker 仍在写库，调用方随后的落盘会记下旧状态，
	// 下次启动重复投递同一条告警。
	DrainTimeout time.Duration
}

// DefaultConfig 返回生产环境参数：最多 5 次尝试（首投 + 4 次重试），
// 退避 30s→30m；MaxAttempts 与 Backoff 长度必须匹配，否则最后一档永远用不到。
func DefaultConfig() Config {
	return Config{
		Workers:     8,
		Tick:        5 * time.Second,
		MaxAttempts: 5,
		Backoff: []time.Duration{
			30 * time.Second,
			2 * time.Minute,
			10 * time.Minute,
			30 * time.Minute,
		},
		// 8 个 worker 各最多一次 12 秒的 HTTP 超时，留一倍余量；
		// 上限 60 秒，避免拖过 systemd 默认的 TimeoutStopSec。
		DrainTimeout: 30 * time.Second,
	}
}

// Manager 是通知队列的调度与投递器。
type Manager struct {
	st  *store.Store
	cfg Config

	notifyCfg atomic.Pointer[notify.Config]
	send      func(*notify.Config, notify.Message) []notify.Result

	wake     chan struct{}
	stop     chan struct{}
	stopOnce sync.Once
	loopWG   sync.WaitGroup
	workWG   sync.WaitGroup

	// startMu 串行化 Start/Stop：保证 loopWG.Add 不会与 Wait 并发
	// （计数为 0 时二者并发属于 WaitGroup 误用，会 panic），
	// 同时让「已 Stop 后再 Start」被明确拒绝。
	startMu sync.Mutex

	mu       sync.Mutex
	inFlight int
	started  atomic.Bool
}

// New 创建投递队列。调用 Start 前不会投递。
func New(st *store.Store, cfg Config) *Manager {
	def := DefaultConfig()
	if cfg.Workers <= 0 {
		cfg.Workers = def.Workers
	}
	if cfg.Tick <= 0 {
		cfg.Tick = def.Tick
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = def.MaxAttempts
	}
	if len(cfg.Backoff) == 0 {
		cfg.Backoff = def.Backoff
	}
	// Config 是按值传入的，但 Backoff 是切片（只复制了头部）：
	// 调用方之后修改自己的切片会改变投递时序，这里复制一份断开共享。
	cfg.Backoff = append([]time.Duration(nil), cfg.Backoff...)
	return &Manager{
		st:   st,
		cfg:  cfg,
		send: notify.Send,
		wake: make(chan struct{}, 1),
		stop: make(chan struct{}),
	}
}

// SetConfig 原子替换通知配置，并唤醒调度器处理已入队任务。
func (m *Manager) SetConfig(cfg *notify.Config) {
	if cfg == nil {
		m.notifyCfg.Store(nil)
		m.signal()
		return
	}
	cp := *cfg
	cp.Channels = make([]notify.ChannelConfig, len(cfg.Channels))
	for i, ch := range cfg.Channels {
		cp.Channels[i] = ch
		if ch.Options != nil {
			opts := make(map[string]string, len(ch.Options))
			for k, v := range ch.Options {
				opts[k] = v
			}
			cp.Channels[i].Options = opts
		}
	}
	m.notifyCfg.Store(&cp)
	m.signal()
}

// Enqueue 把告警消息写入持久队列。通知未启用或严重度不足时直接忽略。
func (m *Manager) Enqueue(msg notify.Message) {
	now := time.Now().Unix()
	cfg := m.notifyCfg.Load()
	if cfg == nil || !cfg.Enabled || len(cfg.EnabledChannels()) == 0 || !cfg.ShouldSend(msg.Severity) {
		return
	}
	eventTS := msg.TS
	if eventTS <= 0 {
		eventTS = now
	}
	job := &model.NotifyJob{
		ID:            store.NewID("nj"),
		AlertID:       msg.AlertID,
		RuleID:        msg.RuleID,
		RuleName:      msg.RuleName,
		NodeID:        msg.NodeID,
		NodeName:      msg.NodeName,
		Severity:      msg.Severity,
		Resolved:      msg.Resolved,
		Title:         msg.Title,
		Body:          msg.Body,
		Value:         msg.Value,
		Link:          msg.Link,
		EventTS:       eventTS,
		CreatedAt:     now,
		NextAttemptAt: now,
		Status:        model.NotifyPending,
	}
	if scope, reason, ok := m.st.SilenceFor(msg.RuleID, now); ok {
		job.Status = model.NotifySuppressed
		job.SuppressedBy = scope
		job.LastError = "通知静默中"
		if reason != "" {
			job.LastError += "：" + reason
		}
	}
	if !m.st.EnqueueNotifyJob(job) {
		log.Printf("通知入队失败，队列已满: %s", msg.Title)
		return
	}
	if job.Status == model.NotifyPending {
		m.signal()
	}
}

// Start 启动调度循环。已经 Stop 的 Manager 不会被重新启动：
// 否则关停后又开始 claim 任务，而 Stop 已返回、没人再等待这些 worker。
func (m *Manager) Start() {
	m.startMu.Lock()
	defer m.startMu.Unlock()
	if m.started.Load() {
		return
	}
	select {
	case <-m.stop:
		return
	default:
	}
	m.loopWG.Add(1)
	m.started.Store(true)
	go m.loop()
}

// Stop 停止调度并等待在途投递完成（最多 20 秒）。
// 未完成的任务保持 pending，重启后会继续重试。
func (m *Manager) Stop() {
	// 持 startMu 关闭 stop：与 Start 的 Add 互斥，
	// 避免「Add 与 Wait 并发且计数为 0」的 WaitGroup 误用。
	m.startMu.Lock()
	m.stopOnce.Do(func() { close(m.stop) })
	m.startMu.Unlock()

	m.loopWG.Wait()
	done := make(chan struct{})
	go func() {
		m.workWG.Wait()
		close(done)
	}()
	budget := m.cfg.DrainTimeout
	if budget <= 0 {
		// 未显式配置时按 worker 数推导：每个 worker 至少可能卡在一个
		// 通道超时上，固定 20 秒在多通道配置下必然不够。
		budget = time.Duration(m.cfg.Workers) * 12 * time.Second
		if budget > 60*time.Second {
			budget = 60 * time.Second
		}
	}
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		m.mu.Lock()
		left := m.inFlight
		m.mu.Unlock()
		log.Printf("通知队列关闭超时（%s），仍有 %d 个投递在途；这些任务保持 pending，重启后会重试",
			budget, left)
	}
}

func (m *Manager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Wake 唤醒调度器立即检查队列，供手动重试后立刻投递。
func (m *Manager) Wake() { m.signal() }

func (m *Manager) loop() {
	defer m.loopWG.Done()
	ticker := time.NewTicker(m.cfg.Tick)
	defer ticker.Stop()
	prune := time.NewTicker(10 * time.Minute)
	defer prune.Stop()

	m.st.PruneNotify(time.Now().Unix())
	if m.stopped() {
		return
	}
	m.dispatch()

	for {
		var timerC <-chan time.Time
		var timer *time.Timer
		if next, ok := m.st.NextNotifyAt(); ok {
			d := time.Until(time.Unix(next, 0))
			if d < time.Second {
				d = time.Second
			}
			timer = time.NewTimer(d)
			timerC = timer.C
		}

		select {
		case <-m.stop:
			if timer != nil {
				timer.Stop()
			}
			return
		case <-timerC:
		case <-m.wake:
		case <-ticker.C:
		case <-prune.C:
			m.st.PruneNotify(time.Now().Unix())
		}
		if timer != nil {
			timer.Stop()
		}
		// select 在多个 case 同时就绪时随机选择：stop 关闭后仍可能走完本轮。
		// 不复查就会在 Stop 返回后继续 claim 任务并起 worker，
		// 而这些 worker 已无人等待，可能边关停边继续外发。
		if m.stopped() {
			return
		}
		m.dispatch()
	}
}

// stopped 报告是否已进入关停状态。
func (m *Manager) stopped() bool {
	select {
	case <-m.stop:
		return true
	default:
		return false
	}
}

func (m *Manager) dispatch() {
	m.mu.Lock()
	free := m.cfg.Workers - m.inFlight
	m.mu.Unlock()
	if free <= 0 {
		return
	}
	jobs := m.st.ClaimNotifyJobs(time.Now().Unix(), free)
	for _, j := range jobs {
		m.mu.Lock()
		m.inFlight++
		m.workWG.Add(1)
		m.mu.Unlock()
		go m.work(j)
	}
}

func (m *Manager) work(job *model.NotifyJob) {
	defer func() {
		m.mu.Lock()
		m.inFlight--
		m.mu.Unlock()
		m.workWG.Done()
		m.signal()
	}()

	now := time.Now().Unix()
	cfg := m.notifyCfg.Load()
	if cfg == nil {
		// 配置尚未装载（启动早期或保存失败）：保持 pending。
		// 不能像「明确停用」那样标记 suppressed —— 那会把这条告警永久丢弃，
		// 而租约到期后任务本可以重新投递。
		return
	}
	if !cfg.Enabled || len(cfg.EnabledChannels()) == 0 {
		m.st.NotifyJobSuppressed(job.ID, "config", "通知配置已停用或没有可用通道", now)
		return
	}
	if !cfg.ShouldSend(job.Severity) {
		m.st.NotifyJobSuppressed(job.ID, "config", "严重度低于当前通知阈值", now)
		return
	}
	if scope, reason, ok := m.st.SilenceFor(job.RuleID, now); ok {
		msg := "通知静默中"
		if reason != "" {
			msg += "：" + reason
		}
		m.st.NotifyJobSuppressed(job.ID, scope, msg, now)
		return
	}

	results := m.send(cfg, notify.Message{
		Title:    job.Title,
		Body:     job.Body,
		Severity: job.Severity,
		NodeName: job.NodeName,
		NodeID:   job.NodeID,
		RuleID:   job.RuleID,
		RuleName: job.RuleName,
		AlertID:  job.AlertID,
		Value:    job.Value,
		Resolved: job.Resolved,
		TS:       job.EventTS,
		Link:     job.Link,
	})
	okAny, errs := resultSummary(results)
	// 发送可能耗时数十秒：状态时间戳与退避基准都要用发送结束后的时间，
	// 否则 NextAttemptAt 会偏早、LastAttempt 也不准。
	now = time.Now().Unix()
	if okAny {
		if errs != "" {
			log.Printf("通知部分通道失败（已至少送达一个通道）: %s", errs)
		}
		m.st.NotifyJobSent(job.ID, errs, now)
		return
	}

	attempts := job.Attempts + 1
	// MaxAttempts 是总尝试次数（含首投），与 Backoff 的档数一一对应。
	if attempts >= m.cfg.MaxAttempts {
		log.Printf("通知投递失败，已重试 %d 次放弃: %s (%s)", attempts, job.Title, errs)
		m.st.NotifyJobFailed(job.ID, errs, now)
		return
	}
	backoff := m.cfg.Backoff[(attempts-1)%len(m.cfg.Backoff)]
	log.Printf("通知投递失败，%s 后重试（第 %d/%d 次）: %s (%s)",
		backoff, attempts, m.cfg.MaxAttempts, job.Title, errs)
	m.st.NotifyJobRetry(job.ID, errs, now+int64(backoff/time.Second))
}

// resultSummary 汇总逐通道结果。任一通道成功即视为已送达，
// 避免为了补发失败通道而向成功通道重复刷屏。
func resultSummary(results []notify.Result) (ok bool, errs string) {
	var failed []string
	for _, r := range results {
		if r.OK {
			ok = true
			continue
		}
		if r.Error == "" {
			continue
		}
		failed = append(failed, fmt.Sprintf("%s: %s", r.Channel, r.Error))
	}
	if len(results) == 0 {
		return false, "没有返回投递结果"
	}
	s := strings.Join(failed, "；")
	r := []rune(s)
	if len(r) > 400 {
		s = string(r[:400]) + "…"
	}
	return ok, s
}

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
	MaxAttempts int           // 单任务最大投递次数
	Backoff     []time.Duration
}

// DefaultConfig 返回生产环境参数：5 次尝试，30s→1h 退避。
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
			time.Hour,
		},
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

// Start 启动调度循环。
func (m *Manager) Start() {
	if !m.started.CompareAndSwap(false, true) {
		return
	}
	m.loopWG.Add(1)
	go m.loop()
}

// Stop 停止调度并等待在途投递完成（最多 20 秒）。
// 未完成的任务保持 pending，重启后会继续重试。
func (m *Manager) Stop() {
	m.stopOnce.Do(func() { close(m.stop) })
	m.loopWG.Wait()
	done := make(chan struct{})
	go func() {
		m.workWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		log.Printf("通知队列关闭超时，未完成任务将在重启后重试")
	}
}

func (m *Manager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) loop() {
	defer m.loopWG.Done()
	ticker := time.NewTicker(m.cfg.Tick)
	defer ticker.Stop()
	prune := time.NewTicker(10 * time.Minute)
	defer prune.Stop()

	m.st.PruneNotify(time.Now().Unix())
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
		m.dispatch()
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
	if cfg == nil || !cfg.Enabled || len(cfg.EnabledChannels()) == 0 {
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
	if okAny {
		if errs != "" {
			log.Printf("通知部分通道失败（已至少送达一个通道）: %s", errs)
		}
		m.st.NotifyJobSent(job.ID, errs, now)
		return
	}

	attempts := job.Attempts + 1
	if attempts >= m.cfg.MaxAttempts {
		log.Printf("通知投递失败，已重试 %d 次放弃: %s (%s)", attempts, job.Title, errs)
		m.st.NotifyJobFailed(job.ID, errs, now)
		return
	}
	backoff := m.cfg.Backoff[(attempts-1)%len(m.cfg.Backoff)]
	log.Printf("通知投递失败，%s 后重试（第 %d/%d 次）: %s (%s)",
		backoff, attempts, m.cfg.MaxAttempts, job.Title, errs)
	m.st.NotifyJobRetry(job.ID, errs, now+int64(backoff/time.Second), now)
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

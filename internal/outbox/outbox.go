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

	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/notify"
	"github.com/motao123/rollcall/internal/store"
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
		// notify.Send 串行遍历通道，单个任务最坏耗时 = 通道数 × 12 秒；
		// 多个 worker 并行，所以预算按「单任务上限」估算。60 秒可覆盖约
		// 5 个通道，同时在 systemd 默认 TimeoutStopSec(90s) 之前留出落盘余量。
		// 超时后未完成的任务保持 pending，下次启动继续重试。
		DrainTimeout: 60 * time.Second,
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

// Enqueue 把告警消息写入持久队列。返回消息是否已被处理：
// 通知停用、没有可用通道、严重度不足或队列已满时返回 false，
// 调用方据此决定是否记录「本期已发送」标记，避免消息被丢弃后当天不再重试。
// 静默中的消息会入队并标记为 suppressed，仍返回 true（重试也不会投递）。
func (m *Manager) Enqueue(msg notify.Message) bool {
	now := time.Now().Unix()
	cfg := m.notifyCfg.Load()
	if cfg == nil || !cfg.Enabled || len(cfg.EnabledChannels()) == 0 || !cfg.ShouldSend(msg.Severity) {
		return false
	}
	eventTS := msg.TS
	if eventTS <= 0 {
		eventTS = now
	}
	next := now
	// 免打扰与摘要只在「非紧急、未恢复」时生效：
	// 恢复通知和 critical 告警延迟到窗口结束后可能已经失去意义。
	if !msg.Resolved && msg.Severity != "critical" {
		if end, quiet := cfg.QuietWindow(time.Now()); quiet {
			next = end.Unix()
		}
		if cfg.DigestMins > 0 {
			if slot := nextDigestSlot(time.Now(), cfg.DigestMins); slot > next {
				next = slot
			}
		}
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
		NextAttemptAt: next,
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
		return false
	}
	if job.Status == model.NotifyPending {
		m.signal()
	}
	return true
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

// Stop 停止调度并等待在途投递完成（上限为 cfg.DrainTimeout，默认 60 秒）。
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
	now := time.Now().Unix()
	jobs := m.st.ClaimNotifyJobs(now, free)
	cfg := m.notifyCfg.Load()

	var digest []*model.NotifyJob
	for _, j := range jobs {
		// 同一轮领取到的多条非紧急告警合并为一条摘要，
		// 避免一次故障引发几十个通道消息刷屏。
		if cfg != nil && cfg.DigestMins > 0 && !j.Resolved && j.Severity != "critical" {
			if _, _, silenced := m.st.SilenceFor(j.RuleID, now); !silenced {
				digest = append(digest, j)
				continue
			}
		}
		m.mu.Lock()
		m.inFlight++
		m.workWG.Add(1)
		m.mu.Unlock()
		go m.work(j)
	}
	if len(digest) == 1 {
		j := digest[0]
		m.mu.Lock()
		m.inFlight++
		m.workWG.Add(1)
		m.mu.Unlock()
		go m.work(j)
		return
	}
	if len(digest) > 1 {
		m.mu.Lock()
		m.inFlight++
		m.workWG.Add(1)
		m.mu.Unlock()
		go m.workDigest(digest)
	}
}

// nextDigestSlot 返回下一个摘要窗口的整点（按 DigestMins 对齐）。
func nextDigestSlot(now time.Time, mins int) int64 {
	step := int64(mins) * 60
	if step <= 0 {
		return now.Unix()
	}
	t := now.Unix()
	return ((t + step - 1) / step) * step
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
	if !cfg.Enabled {
		m.st.NotifyJobSuppressed(job.ID, "config", "通知已停用", now)
		return
	}
	// 「启用中但没有可用通道」通常是配置写错（缺必填项）或全部通道被临时
	// 关闭，是可恢复状态：像配置未加载一样保持 pending，租约到期后重试。
	// 若在这里标记 suppressed，恢复通道后这些告警会被永久丢弃。
	if len(cfg.EnabledChannels()) == 0 {
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

	// 免打扰：配置可能在入队后变更，投递前再检查一次。
	// 用 Defer 而非 Retry：推迟不应消耗重试次数。
	if !job.Resolved && job.Severity != "critical" {
		if end, quiet := cfg.QuietWindow(time.Now()); quiet {
			m.st.NotifyJobDefer(job.ID, end.Unix())
			return
		}
	}

	results := m.send(cfg, jobMessage(job))
	okAny, errs := resultSummary(results)
	// 发送可能耗时数十秒：状态时间戳与退避基准都要用发送结束后的时间，
	// 否则 NextAttemptAt 会偏早、LastAttempt 也不准。
	now = time.Now().Unix()
	m.finish(job, okAny, errs, now)
}

func jobMessage(job *model.NotifyJob) notify.Message {
	return notify.Message{
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
	}
}

// finish 根据发送结果更新任务状态。
func (m *Manager) finish(job *model.NotifyJob, okAny bool, errs string, now int64) {
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

// workDigest 把同一窗口内到期的多条告警合并为一条摘要投递。
func (m *Manager) workDigest(jobs []*model.NotifyJob) {
	defer func() {
		m.mu.Lock()
		m.inFlight--
		m.mu.Unlock()
		m.workWG.Done()
		m.signal()
	}()

	cfg := m.notifyCfg.Load()
	if cfg == nil {
		return
	}
	if !cfg.Enabled {
		now := time.Now().Unix()
		for _, j := range jobs {
			m.st.NotifyJobSuppressed(j.ID, "config", "通知已停用", now)
		}
		return
	}
	if len(cfg.EnabledChannels()) == 0 {
		return
	}

	// 投递前重新校验每条任务；期间配置可能变化，也可能被静默。
	sendable := make([]*model.NotifyJob, 0, len(jobs))
	for _, j := range jobs {
		now := time.Now().Unix()
		if !cfg.ShouldSend(j.Severity) {
			m.st.NotifyJobSuppressed(j.ID, "config", "严重度低于当前通知阈值", now)
			continue
		}
		if scope, reason, ok := m.st.SilenceFor(j.RuleID, now); ok {
			msg := "通知静默中"
			if reason != "" {
				msg += "：" + reason
			}
			m.st.NotifyJobSuppressed(j.ID, scope, msg, now)
			continue
		}
		if end, quiet := cfg.QuietWindow(time.Now()); quiet {
			m.st.NotifyJobDefer(j.ID, end.Unix())
			continue
		}
		sendable = append(sendable, j)
	}
	if len(sendable) == 0 {
		return
	}

	var results []notify.Result
	if len(sendable) == 1 {
		results = m.send(cfg, jobMessage(sendable[0]))
	} else {
		results = m.send(cfg, digestMessage(sendable))
	}
	okAny, errs := resultSummary(results)
	now := time.Now().Unix()
	for _, j := range sendable {
		m.finish(j, okAny, errs, now)
	}
}

func digestMessage(jobs []*model.NotifyJob) notify.Message {
	rank := 0
	sev := "info"
	var b strings.Builder
	var link string
	for i, j := range jobs {
		if r := notify.SeverityRank(j.Severity); r > rank {
			rank, sev = r, j.Severity
		}
		if link == "" {
			link = j.Link
		}
		if i >= 50 {
			fmt.Fprintf(&b, "…另有 %d 条告警未展开\n", len(jobs)-50)
			break
		}
		fmt.Fprintf(&b, "- [%s] %s · %s", j.Severity, j.NodeName, j.Title)
		if j.Value != 0 {
			fmt.Fprintf(&b, "（%.1f）", j.Value)
		}
		b.WriteString("\n")
	}
	names := make([]string, 0, len(jobs))
	seen := map[string]bool{}
	for _, j := range jobs {
		if j.NodeName != "" && !seen[j.NodeName] {
			seen[j.NodeName] = true
			names = append(names, j.NodeName)
		}
	}
	nodeLabel := "多个节点"
	if len(names) == 1 {
		nodeLabel = names[0]
	}
	if len(names) > 1 {
		nodeLabel = fmt.Sprintf("%s 等 %d 个节点", names[0], len(names))
	}
	return notify.Message{
		Title:    fmt.Sprintf("%d 条告警汇总", len(jobs)),
		Body:     strings.TrimRight(b.String(), "\n"),
		Severity: sev,
		NodeName: nodeLabel,
		RuleName: "聚合摘要",
		TS:       time.Now().Unix(),
		Link:     link,
	}
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

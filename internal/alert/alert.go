// Package alert 实现告警评估引擎。
//
// 策略：每 15 秒评估一轮，规则需持续满足 Duration 秒才触发，避免抖动误报。
package alert

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/motao/rollcall/internal/model"
	"github.com/motao/rollcall/internal/notify"
	"github.com/motao/rollcall/internal/store"
)

type Engine struct {
	st     *store.Store
	mu     sync.Mutex
	since  map[string]int64        // ruleID|nodeID -> 首次越界时间
	active map[string]*model.Alert // 已在告警中的条目
	lastAt map[string]int64        // 上次通知时间，用于重复提醒
	notify func(notify.Message)    // 投递回调（nil 表示不通知）
	repeat int                     // 重复通知间隔（秒），0=不重复
	link   string                  // 面板地址，拼进通知
}

func New(st *store.Store) *Engine {
	return &Engine{
		st:     st,
		since:  map[string]int64{},
		active: map[string]*model.Alert{},
		lastAt: map[string]int64{},
	}
}

// SetNotifier 注入通知投递回调。
func (e *Engine) SetNotifier(fn func(notify.Message)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.notify = fn
}

// SetRepeat 设置重复通知间隔（秒）。
func (e *Engine) SetRepeat(sec int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.repeat = sec
}

// SetLink 设置通知里附带的面板地址。
func (e *Engine) SetLink(link string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.link = link
}

// send 组装并投递通知。
// 取回调与 link 时短暂持锁，随即释放再调用 fn —— 投递可能耗时十余秒，
// 绝不能持锁进行，否则评估循环会被阻塞。调用方同样不得持锁。
func (e *Engine) send(a *model.Alert, resolved bool) {
	e.mu.Lock()
	fn, link := e.notify, e.link
	if fn == nil {
		e.mu.Unlock()
		return
	}
	if resolved {
		delete(e.lastAt, a.RuleID+"|"+a.NodeID)
	}
	e.mu.Unlock()

	fn(notify.Message{
		Title:    a.RuleName,
		Severity: a.Severity,
		NodeName: a.NodeName,
		NodeID:   a.NodeID,
		RuleName: a.RuleName,
		Value:    a.Value,
		Resolved: resolved,
		TS:       time.Now().Unix(),
		Link:     link,
		Body:     a.Message,
	})
}

// Start 启动后台评估循环。
func (e *Engine) Start(every time.Duration) {
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for range t.C {
			e.Evaluate()
		}
	}()
}

// Evaluate 执行一轮评估。
func (e *Engine) Evaluate() {
	nodes := e.st.Nodes()
	rules := e.st.Rules()
	now := time.Now().Unix()

	// 本轮实际评估到的 key，用于清理引擎状态：
	// 节点被删除或规则被停用后，它们不再进入下面的循环，
	// 其 since/active/lastAt 条目会永久残留（内存泄漏），
	// 且停用规则名下的活跃告警会永远挂在「进行中」无法自愈。
	seen := make(map[string]struct{}, len(rules)*len(nodes))

	for _, rule := range rules {
		if !rule.Enabled {
			// 停用规则：关闭其遗留的活跃告警，避免僵尸条目
			if e.st.ResolveRuleAlerts(rule.ID) > 0 {
				e.mu.Lock()
				for k := range e.active {
					if strings.HasPrefix(k, rule.ID+"|") {
						delete(e.active, k)
						delete(e.since, k)
						delete(e.lastAt, k)
					}
				}
				e.mu.Unlock()
			}
			continue
		}
		for _, n := range nodes {
			key := rule.ID + "|" + n.ID
			seen[key] = struct{}{}
			val, breached := e.check(rule, n.ID, now)

			if !breached {
				e.mu.Lock()
				delete(e.since, key)
				_, wasActive := e.active[key]
				delete(e.active, key)
				e.mu.Unlock()

				e.st.ResolveAlert(rule.ID, n.ID)
				if wasActive {
					e.send(&model.Alert{
						RuleID: rule.ID, RuleName: rule.Name,
						NodeID: n.ID, NodeName: n.Name,
						Severity: rule.Severity, Value: val,
						Message: fmt.Sprintf("%s 的「%s」已恢复", n.Name, rule.Name),
					}, true)
				}
				continue
			}

			// 记录首次越界时间，满足持续时长才告警
			e.mu.Lock()
			first, ok := e.since[key]
			if !ok {
				e.since[key] = now
				first = now
			}
			e.mu.Unlock()

			if now-first < int64(rule.Duration) {
				continue
			}

			a := &model.Alert{
				ID:        fmt.Sprintf("al_%d_%s_%s", now, rule.ID, n.ID),
				RuleID:    rule.ID,
				RuleName:  rule.Name,
				NodeID:    n.ID,
				NodeName:  n.Name,
				Severity:  rule.Severity,
				Value:     val,
				Message:   describe(rule, n.Name, val),
				StartedAt: first,
			}
			isNew := e.st.OpenAlert(a)

			// 通知：新告警立即发，已在告警中的按 repeat 间隔重发
			e.mu.Lock()
			_, tracked := e.active[key]
			last := e.lastAt[key]
			repeat := e.repeat
			e.mu.Unlock()

			fire := isNew || !tracked
			if !fire && repeat > 0 && now-last >= int64(repeat) {
				fire = true
			}
			if fire {
				e.mu.Lock()
				e.active[key] = a
				e.lastAt[key] = now
				e.mu.Unlock()
				e.send(a, false)
			} else if !tracked {
				e.mu.Lock()
				e.active[key] = a
				e.mu.Unlock()
			}
		}
	}

	// 清理本轮未评估到的 key（节点被删或规则被停用/删除）。
	// 否则 since/active/lastAt 会随节点与规则的增删无限增长。
	e.mu.Lock()
	for k := range e.since {
		if _, ok := seen[k]; !ok {
			delete(e.since, k)
		}
	}
	for k := range e.active {
		if _, ok := seen[k]; !ok {
			delete(e.active, k)
			delete(e.lastAt, k)
		}
	}
	for k := range e.lastAt {
		if _, ok := seen[k]; !ok {
			delete(e.lastAt, k)
		}
	}
	e.mu.Unlock()
}

// check 返回当前值与是否越界。
func (e *Engine) check(rule *model.AlertRule, nodeID string, now int64) (float64, bool) {
	m, ts, ok := e.st.Latest(nodeID)

	// 离线判定与指标无关
	if rule.Metric == "offline" {
		// 从未上报过的节点：ts 为 0，不能直接算 now-ts（会得到整个 Unix 时间戳）
		if !ok {
			return 0, true
		}
		if now-ts > store.OnlineWindow {
			return float64(now - ts), true
		}
		return 0, false
	}

	if !ok || now-ts > store.OnlineWindow {
		return 0, false // 已离线时不重复触发指标类告警
	}

	var v float64
	switch rule.Metric {
	case "cpu":
		v = m.CPUPct
	case "mem":
		v = pct(m.MemUsed, m.MemTotal)
	case "disk":
		v = pct(m.DiskUsed, m.DiskTotal)
	case "load":
		v = m.Load1
	default:
		return 0, false
	}

	switch rule.Op {
	case "lt":
		return v, v < rule.Threshold
	default: // gt
		return v, v > rule.Threshold
	}
}

func pct(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) / float64(total) * 100
}

func describe(rule *model.AlertRule, node string, v float64) string {
	// op 决定措辞：lt 规则（例如"可用率低于 X"）说"超过阈值"是错的
	rel := "超过"
	if rule.Op == "lt" {
		rel = "低于"
	}
	switch rule.Metric {
	case "offline":
		if v <= 0 {
			return fmt.Sprintf("%s 从未上报过数据（Agent 未部署或配置有误）", node)
		}
		return fmt.Sprintf("%s 已失联 %s", node, humanDur(int64(v)))
	case "cpu", "mem", "disk":
		return fmt.Sprintf("%s %s 使用率 %.1f%%，%s阈值 %.0f%%",
			node, metricName(rule.Metric), v, rel, rule.Threshold)
	case "load":
		return fmt.Sprintf("%s 1 分钟负载 %.2f，%s阈值 %.2f", node, v, rel, rule.Threshold)
	}
	return fmt.Sprintf("%s %s = %.2f", node, rule.Metric, v)
}

func metricName(m string) string {
	switch m {
	case "cpu":
		return "CPU"
	case "mem":
		return "内存"
	case "disk":
		return "磁盘"
	}
	return m
}

// DefaultRules 返回一套开箱即用的默认规则。
func DefaultRules() []*model.AlertRule {
	return []*model.AlertRule{
		{ID: "r_offline", Name: "节点失联", Metric: "offline", Op: "gt", Threshold: 1, Duration: 180, Severity: "critical", Enabled: true},
		{ID: "r_cpu", Name: "CPU 持续高负载", Metric: "cpu", Op: "gt", Threshold: 90, Duration: 300, Severity: "warn", Enabled: true},
		{ID: "r_mem", Name: "内存使用率过高", Metric: "mem", Op: "gt", Threshold: 90, Duration: 300, Severity: "warn", Enabled: true},
		{ID: "r_disk", Name: "磁盘空间不足", Metric: "disk", Op: "gt", Threshold: 85, Duration: 600, Severity: "critical", Enabled: true},
	}
}

// humanDur 把秒数转成「3 分钟」「2 小时」这类可读描述。
func humanDur(sec int64) string {
	switch {
	case sec < 60:
		return fmt.Sprintf("%d 秒", sec)
	case sec < 3600:
		return fmt.Sprintf("%d 分钟", sec/60)
	case sec < 86400:
		return fmt.Sprintf("%d 小时 %d 分钟", sec/3600, (sec%3600)/60)
	default:
		return fmt.Sprintf("%d 天 %d 小时", sec/86400, (sec%86400)/3600)
	}
}

// Package alert 实现告警评估引擎。
//
// 策略：每 15 秒评估一轮，规则需持续满足 Duration 秒才触发，避免抖动误报。
// 离线规则另有抖动抑制：刚恢复又立刻掉线的节点要满更长的时长才告警，
// 否则一次网络抖动会变成"掉线—恢复—掉线"的通知刷屏。
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

const (
	// flapWindow 是抖动窗口：一次恢复之后这么久内再次开始的离线，
	// 要满 flapGrace 才进入告警。
	flapWindow int64 = 900 // 15 分钟
	// flapGrace 是抖动窗口内的最短持续时长。
	flapGrace int64 = 300 // 5 分钟
)

// 窗口必须严格大于宽限，否则"窗口内再掉"与"抖动宽限"两个量失去各自的含义。
// 常量表达式在编译期钉住这条关系：差为负时转 uint64 溢出，编译失败。
const _ = uint64(flapWindow - flapGrace - 1)

type Engine struct {
	st     *store.Store
	evalMu sync.Mutex // 保证 Evaluate 单飞
	mu     sync.Mutex
	since  map[string]int64        // ruleID|nodeID -> 首次越界时间
	active map[string]*model.Alert // 已在告警中的条目
	lastAt map[string]int64        // 上次通知时间，用于重复提醒
	recAt  map[string]int64        // ruleID|nodeID -> 最近一次恢复时间，用于抖动抑制
	notify func(notify.Message)    // 投递回调（nil 表示不通知）
	repeat int                     // 重复通知间隔（秒），0=不重复
	link   string                  // 面板地址，拼进通知
	clk    func() int64            // 取当前 Unix 秒，测试可替换
}

// SetClock 替换时间源。
//
// 抖动抑制、持续时长、重复提醒全都是"距上次事件过了多久"的判定，
// 靠真实时钟就只能靠 sleep 测，既慢又不稳定。注入后这些分支可以在
// 确定的时间点上直接验证。
func (e *Engine) SetClock(fn func() int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.clk = fn
}

// now 取当前 Unix 秒。调用方不得持 e.mu。
func (e *Engine) now() int64 {
	e.mu.Lock()
	fn := e.clk
	e.mu.Unlock()
	if fn == nil {
		return time.Now().Unix()
	}
	return fn()
}

func New(st *store.Store) *Engine {
	e := &Engine{
		st:     st,
		since:  map[string]int64{},
		active: map[string]*model.Alert{},
		lastAt: map[string]int64{},
		recAt:  map[string]int64{},
	}
	// 从持久化的未恢复告警恢复引擎状态：
	// 否则重启后首轮评估会把这些告警当成"新告警"再通知一遍。
	// lastAt 设为当前时间，让重复提醒从重启时刻重新计时。
	now := time.Now().Unix()
	for _, a := range st.ActiveAlerts() {
		key := a.RuleID + "|" + a.NodeID
		e.active[key] = a
		e.since[key] = a.StartedAt
		e.lastAt[key] = now
	}
	return e
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
	// 时间戳在加锁前取：e.now() 自己要加锁，持锁调用会自死锁。
	ts := e.now()
	e.mu.Lock()
	fn, link := e.notify, e.link
	if resolved {
		// 已恢复的条目必须无条件清掉，包括通知器为 nil 的情况
		// （engine.Start 早于 web.New 装通知器、或 Outbox=nil 时会被置空）。
		// 放在 nil 判断之后 return 会让 lastAt 永久残留，同一 key 下次越界
		// 时就会拿一个陈旧时间做「重复提醒」判断。
		delete(e.lastAt, a.RuleID+"|"+a.NodeID)
	}
	e.mu.Unlock()
	if fn == nil {
		return
	}

	fn(notify.Message{
		Title:    a.RuleName,
		Severity: a.Severity,
		NodeName: a.NodeName,
		NodeID:   a.NodeID,
		RuleID:   a.RuleID,
		RuleName: a.RuleName,
		AlertID:  a.ID,
		Value:    a.Value,
		Resolved: resolved,
		TS:       ts,
		Link:     link,
		Body:     a.Message,
	})
}

// RecoverClosed 为因管理操作关闭的告警补发恢复通知，并清理引擎内跟踪状态。
// Evaluate 只处理启用中的规则，规则停用/删除/改向关闭的告警否则会静默消失。
//
// 与 Evaluate 串行：管理路径（删除/停用规则）可能在任意时刻调用，
// 若与一轮评估交错，评估可能在这条告警被关闭之后又把它写回 active，
// 留下永远无法自愈的幽灵条目。
func (e *Engine) RecoverClosed(alerts []*model.Alert, reason string) {
	e.evalMu.Lock()
	defer e.evalMu.Unlock()
	e.recoverClosedLocked(alerts, reason, "")
}

// RecoverRuleClosed 关闭整条规则的告警并清空它的全部引擎状态。
//
// 用于规则被删除或停用：这类规则不会再进入 Evaluate，
// 任何残留条目都失去清理路径。传 ruleID 而不是只传快照，是为了覆盖
// 「取快照」与「规则真正删除」之间新开的告警。
func (e *Engine) RecoverRuleClosed(ruleID string, alerts []*model.Alert, reason string) {
	e.evalMu.Lock()
	defer e.evalMu.Unlock()
	e.recoverClosedLocked(alerts, reason, ruleID)
}

// recoverClosedLocked 要求调用方已持有 evalMu（Evaluate 内部路径直接调用，
// 否则会在已经持锁的情况下二次加锁而自死锁）。
// purgeRuleID 非空时按规则前缀清空全部条目；否则只清快照里那些精确 key
// （规则只是收窄范围时，仍留在范围内的节点必须保留跟踪状态）。
func (e *Engine) recoverClosedLocked(alerts []*model.Alert, reason, purgeRuleID string) {
	var belongs func(string) bool
	if purgeRuleID != "" {
		prefix := purgeRuleID + "|"
		belongs = func(key string) bool { return strings.HasPrefix(key, prefix) }
	} else {
		keys := make(map[string]struct{}, len(alerts))
		for _, a := range alerts {
			if a != nil {
				keys[a.RuleID+"|"+a.NodeID] = struct{}{}
			}
		}
		belongs = func(key string) bool {
			_, ok := keys[key]
			return ok
		}
	}
	if len(alerts) > 0 || purgeRuleID != "" {
		e.mu.Lock()
		for k := range e.active {
			if belongs(k) {
				delete(e.active, k)
			}
		}
		for k := range e.since {
			if belongs(k) {
				delete(e.since, k)
			}
		}
		for k := range e.lastAt {
			if belongs(k) {
				delete(e.lastAt, k)
			}
		}
		// 恢复历史一并清掉：管理员手工关闭与规则停用都不是节点真正恢复，
		// 留着一条陈旧恢复时间会让下次掉线误判成"刚恢复又掉"的抖动。
		for k := range e.recAt {
			if belongs(k) {
				delete(e.recAt, k)
			}
		}
		e.mu.Unlock()
	}

	for _, a := range alerts {
		if a == nil {
			continue
		}
		cp := *a
		cp.Message = fmt.Sprintf("%s 的「%s」已恢复（%s）", a.NodeName, a.RuleName, reason)
		e.send(&cp, true)
	}
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

// requiredDuration 返回这条规则本次需要持续满足的秒数。
//
// ignoreFlap 为真时关闭抖动抑制。它只用于派生 FlapDeferred：判定逻辑始终
// 只写在这一处，派生判定不去复述 TTL、宽限、窗口任何一道闸门，
// 以后加门或改门，派生判定自动跟上，不会留下两份漂移的口径。
func (e *Engine) requiredDuration(rule *model.AlertRule, key string, now int64, ignoreFlap bool) int64 {
	need := int64(rule.Duration)
	if need < 0 {
		need = 0
	}
	// 抖动抑制只压离线的噪声：资源类规则本来就要持续满足才触发，
	// 再叠加一层宽限只会拖慢真实告警。
	if ignoreFlap || rule.Metric != "offline" {
		return need
	}
	e.mu.Lock()
	rec, ok := e.recAt[key]
	e.mu.Unlock()
	if !ok || now-rec >= flapWindow {
		return need
	}
	if need < flapGrace {
		return flapGrace
	}
	return need
}

// FlapDeferred 报告这一轮是否只因抖动抑制而停在未触发：
// 拿掉恢复历史之后本会触发，带上之后还不触发。
//
// 字面意思就是它的定义，所以实现就是跑两遍同一个判定、比较结果——
// 而不是把"窗口内再掉""抖动宽限"这些条件再写一遍。
func (e *Engine) FlapDeferred(rule *model.AlertRule, nodeID string, first, now int64) bool {
	key := rule.ID + "|" + nodeID
	without := now-first >= e.requiredDuration(rule, key, now, true)
	with := now-first >= e.requiredDuration(rule, key, now, false)
	return without && !with
}

// Evaluate 执行一轮评估。
func (e *Engine) Evaluate() {
	e.evalMu.Lock()
	defer e.evalMu.Unlock()

	nodes := e.st.Nodes()
	rules := e.st.Rules()
	now := e.now()

	// 计划维护窗口内的节点不做评估：计划内重启会让"节点失联"规则刷一屏告警，
	// 这些告警既无用又会掩盖真正的故障。已在维护中的节点，
	// 其既有活跃告警以"计划维护"为由关闭，避免挂到窗口结束才自愈。
	maintGlobal, maintNodes := e.st.MaintenanceAt(now)

	// 本轮实际评估到的 key，用于清理引擎状态：
	// 节点被删除或规则被停用后，它们不再进入下面的循环，
	// 其 since/active/lastAt 条目会永久残留（内存泄漏），
	// 且停用规则名下的活跃告警会永远挂在「进行中」无法自愈。
	seen := make(map[string]struct{}, len(rules)*len(nodes))

	for _, rule := range rules {
		if !rule.Enabled {
			// 停用规则：关闭遗留活跃告警并补发恢复通知，避免静默消失
			e.recoverClosedLocked(e.st.ResolveRuleAlerts(rule.ID), "规则已停用", rule.ID)
			continue
		}
		for _, n := range nodes {
			key := rule.ID + "|" + n.ID
			// 全局窗口覆盖所有节点；节点级窗口优先用于展示更具体的标题。
			m := maintNodes[n.ID]
			if m == nil {
				m = maintGlobal
			}
			if m != nil {
				// 计划维护：关掉该节点在此规则下的活跃告警并清空引擎状态。
				// 这里必须走与"规则范围调整"相同的收尾路径，否则 since/active
				// 会残留，窗口结束后同一条告警被判成"已经持续很久"而立即触发。
				e.mu.Lock()
				prev := e.active[key]
				delete(e.since, key)
				delete(e.active, key)
				delete(e.lastAt, key)
				delete(e.recAt, key)
				e.mu.Unlock()
				if e.st.ResolveAlert(rule.ID, n.ID) {
					if prev == nil {
						prev = &model.Alert{
							RuleID: rule.ID, RuleName: rule.Name,
							NodeID: n.ID, NodeName: n.Name,
							Severity: rule.Severity,
						}
					}
					e.recoverClosedLocked([]*model.Alert{prev}, maintenanceReason(m), "")
				}
				continue
			}
			if !ruleApplies(rule, n) {
				// 规则改为定向后，原先命中但已不在范围内的节点需要关闭告警，
				// 否则会留下永远无人恢复的「进行中」条目。
				e.mu.Lock()
				prev := e.active[key]
				delete(e.since, key)
				delete(e.active, key)
				delete(e.lastAt, key)
				delete(e.recAt, key)
				e.mu.Unlock()
				if e.st.ResolveAlert(rule.ID, n.ID) {
					if prev == nil {
						prev = &model.Alert{
							RuleID: rule.ID, RuleName: rule.Name,
							NodeID: n.ID, NodeName: n.Name,
							Severity: rule.Severity,
						}
					}
					e.recoverClosedLocked([]*model.Alert{prev}, "规则范围已调整", "")
				}
				continue
			}
			seen[key] = struct{}{}
			val, breached := e.check(rule, n.ID, now)

			if !breached {
				e.mu.Lock()
				delete(e.since, key)
				prev, wasActive := e.active[key]
				delete(e.active, key)
				if wasActive {
					// 记下恢复时刻，供抖动抑制使用：下一次再掉线时据此判断
					// 是不是"刚恢复又掉"的抖动。未真正告警过的（wasActive 为假）
					// 不算一次恢复，否则会平白抬高下一次的门槛。
					e.recAt[key] = now
				}
				e.mu.Unlock()

				resolved := e.st.ResolveAlert(rule.ID, n.ID)
				if wasActive || resolved {
					a := &model.Alert{
						RuleID: rule.ID, RuleName: rule.Name,
						NodeID: n.ID, NodeName: n.Name,
						Severity: rule.Severity, Value: val,
						Message: fmt.Sprintf("%s 的「%s」已恢复", n.Name, rule.Name),
					}
					if prev != nil {
						a.ID = prev.ID
					}
					e.send(a, true)
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

			need := e.requiredDuration(rule, key, now, false)
			if now-first < need {
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
			isNew, exists := e.st.OpenAlert(a)
			if !exists {
				// 节点或规则在评估期间被删除，跳过，避免留下僵尸告警
				continue
			}

			// 通知：新告警立即发，已在告警中的按 repeat 间隔重发。
			// 重启后 Store 里仍有未恢复告警、但内存 active 为空时，
			// 只恢复跟踪状态，不能当成新告警重复通知。
			e.mu.Lock()
			_, tracked := e.active[key]
			last := e.lastAt[key]
			repeat := e.repeat
			if !tracked && !isNew {
				e.active[key] = a
				e.lastAt[key] = now
			}
			e.mu.Unlock()

			fire := isNew
			if tracked && !fire && repeat > 0 && now-last >= int64(repeat) {
				fire = true
			}
			if fire {
				e.mu.Lock()
				e.active[key] = a
				e.lastAt[key] = now
				e.mu.Unlock()
				e.send(a, false)
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
	for k := range e.recAt {
		if _, ok := seen[k]; !ok {
			delete(e.recAt, k)
		}
	}
	e.mu.Unlock()
}

// ruleApplies 判断规则是否作用于该节点。
// NodeIDs 与 Groups 都为空表示全部节点；否则按节点 ID 或分组做「或」匹配。
func ruleApplies(rule *model.AlertRule, n *model.Node) bool {
	if len(rule.NodeIDs) == 0 && len(rule.Groups) == 0 {
		return true
	}
	for _, id := range rule.NodeIDs {
		if id == n.ID {
			return true
		}
	}
	if n.Group != "" {
		for _, g := range rule.Groups {
			if g == n.Group {
				return true
			}
		}
	}
	return false
}

// maintenanceReason 生成维护窗口的恢复原因。
// 带上窗口标题，让恢复通知能说清"因为哪次维护"，而不是一句笼统的"计划维护"。
func maintenanceReason(m *model.Maintenance) string {
	if m == nil || m.Title == "" {
		return "计划维护"
	}
	return "计划维护：" + m.Title
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

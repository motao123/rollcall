// Package schedule 运行与告警引擎无关的定时任务：
// 资产到期提醒与每日/每周运行报告。
package schedule

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/notify"
	"github.com/motao123/rollcall/internal/outbox"
	"github.com/motao123/rollcall/internal/store"
)

// Config 是定时任务的运行参数。
type Config struct {
	PanelLink string
	CNYRate   float64
}

// Runner 是定时任务调度器。
type Runner struct {
	st  *store.Store
	ob  *outbox.Manager
	cfg Config

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

func New(st *store.Store, ob *outbox.Manager, cfg Config) *Runner {
	if cfg.CNYRate <= 0 {
		cfg.CNYRate = 7.2
	}
	return &Runner{st: st, ob: ob, cfg: cfg, stop: make(chan struct{})}
}

func (r *Runner) Start() {
	if r.ob == nil {
		return
	}
	r.wg.Add(1)
	go r.loop()
}

func (r *Runner) Stop() {
	r.stopOnce.Do(func() { close(r.stop) })
	r.wg.Wait()
}

func (r *Runner) loop() {
	defer r.wg.Done()
	// 启动后先跑一次到期检查：进程可能已停机数天，等到下一个整点
	// 才提醒可能已经错过续费。
	r.expiryTick()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case now := <-ticker.C:
			if now.Minute()%10 == 0 {
				r.expiryTick()
			}
			r.reportTick(now)
		}
	}
}

// notifyCfg 每次从设置读取，保存后无需重启即可生效。
func (r *Runner) notifyCfg() *notify.Config {
	cfg, err := notify.Parse(r.st.GetSetting("notify"))
	if err != nil {
		// 不能静默：配置一坏，到期提醒与所有报告都会停摆，
		// 而日志里什么都没有，排查时完全看不出原因。
		log.Printf("解析通知配置失败，跳过到期提醒与运行报告: %v", err)
		return nil
	}
	return cfg
}

// pruneReportMarkers 清理过期的报告去重键。
//
// 去重键是 report.daily.YYYY-MM-DD / report.weekly.YYYY-Www / report.monthly.YYYY-MM，
// 每期一个且永不过期，长期运行会让 settings 无界增长（也拖慢快照序列化）。
// 去重只需要当期键，因此保留近两年、更早的删除。
func (r *Runner) pruneReportMarkers(now time.Time) {
	const dailyPrefix = "report.daily."
	dailyCut := now.AddDate(0, 0, -120).Format("2006-01-02")
	for _, k := range r.st.SettingKeys(dailyPrefix) {
		if v := strings.TrimPrefix(k, dailyPrefix); v != k && v < dailyCut {
			r.st.DeleteSetting(k)
		}
	}

	const monthPrefix = "report.monthly."
	monthCut := now.AddDate(-2, 0, 0).Format("2006-01")
	for _, k := range r.st.SettingKeys(monthPrefix) {
		if v := strings.TrimPrefix(k, monthPrefix); v != k && v < monthCut {
			r.st.DeleteSetting(k)
		}
	}

	const weekPrefix = "report.weekly."
	cy, cw := now.AddDate(-2, 0, 0).ISOWeek()
	cut := cy*100 + cw
	for _, k := range r.st.SettingKeys(weekPrefix) {
		y, w, ok := parseISOWeekKey(strings.TrimPrefix(k, weekPrefix))
		if ok && y*100+w < cut {
			r.st.DeleteSetting(k)
		}
	}
}

// parseISOWeekKey 解析 "2025-W01"；格式不符时 ok=false（保持原样，不误删）。
func parseISOWeekKey(v string) (year, week int, ok bool) {
	if len(v) != 8 || v[4] != '-' || v[5] != 'W' {
		return 0, 0, false
	}
	y, err1 := strconv.Atoi(v[:4])
	w, err2 := strconv.Atoi(v[6:])
	if err1 != nil || err2 != nil || y < 2000 || w < 1 || w > 53 {
		return 0, 0, false
	}
	return y, w, true
}

func (r *Runner) expiryTick() {
	// 顺带清理报告去重键（每小时一次即可，这里按 10 分钟节流调用，成本极低）
	r.pruneReportMarkers(time.Now())

	cfg := r.notifyCfg()
	if cfg == nil || !cfg.Enabled || cfg.ExpiryDays < 0 {
		return
	}
	now := time.Now()
	day := now.Format("2006-01-02")
	for _, n := range r.st.Nodes() {
		if strings.TrimSpace(n.ExpireAt) == "" {
			continue
		}
		days, ok := daysUntil(n.ExpireAt, now)
		if !ok || days < 0 || days > cfg.ExpiryDays {
			continue
		}
		if r.st.ExpirySentFor(n.ID, day) {
			continue
		}
		level := "warn"
		title := "资产即将到期"
		if days == 0 {
			title = "资产今天到期"
			level = "critical"
		}
		body := fmt.Sprintf("到期日期：%s，剩余 %d 天。请及时续费，避免服务中断。", n.ExpireAt, days)
		// 只有消息真正进了队列才记「今天已提醒」：
		// 队列满或没有可用通道时先不标记，下个 tick 还能补发。
		if !r.ob.Enqueue(notify.Message{
			Title:    title,
			Body:     body,
			Severity: level,
			NodeName: n.Name,
			NodeID:   n.ID,
			RuleID:   "expiry",
			RuleName: "资产到期提醒",
			TS:       now.Unix(),
			Link:     r.cfg.PanelLink,
		}) {
			continue
		}
		r.st.MarkExpirySent(n.ID, day)
		r.st.AddEvent(model.Event{
			Kind: "expiry", Level: level, NodeID: n.ID, NodeName: n.Name,
			Text: fmt.Sprintf("%s 将在 %d 天后到期（%s）", n.Name, days, n.ExpireAt),
		})
	}
}

func (r *Runner) reportTick(now time.Time) {
	cfg := r.notifyCfg()
	if cfg == nil || !cfg.Enabled {
		return
	}
	// 用 >= 而不是 ==：进程可能在报告时刻没在运行（重启/升级/时钟跳变），
	// 用等号会让整期报告静默丢失，与"当天内仍可补发"的约定不符。
	// 去重键保证一个周期只会真正入队一次。
	if cfg.ReportDaily && now.Hour() >= cfg.ReportHour {
		key := "report.daily." + now.Format("2006-01-02")
		if r.st.GetSetting(key) == "" && r.sendReport(cfg, "daily", now) {
			r.st.SetSetting(key, "1")
		}
	}
	// 周报在周一发送；ReportHour 之前不提前，错过整点后当天内仍可补发。
	if cfg.ReportWeek && now.Weekday() == time.Monday && now.Hour() >= cfg.ReportHour {
		// ★ 必须用 ISOWeek 返回的 ISO 年，不能用日历年：
		// 2024-12-30 属于 ISO 2025-W01，用 now.Year() 会算成 2024-W01，
		// 与 2024-01-01 的键碰撞，导致当周周报被判为"已发送"而整周不发。
		isoYear, week := now.ISOWeek()
		key := fmt.Sprintf("report.weekly.%d-W%02d", isoYear, week)
		if r.st.GetSetting(key) == "" && r.sendReport(cfg, "weekly", now) {
			r.st.SetSetting(key, "1")
		}
	}
	// 月报在每月 1 日发送；与日报同一时刻时两条独立入队。
	if cfg.ReportMonth && now.Day() == 1 && now.Hour() >= cfg.ReportHour {
		key := "report.monthly." + now.Format("2006-01")
		if r.st.GetSetting(key) == "" && r.sendReport(cfg, "monthly", now) {
			r.st.SetSetting(key, "1")
		}
	}
}

// sendReport 生成并投递运行报告，返回消息是否已进入通知队列。
// 入队失败（队列满/无可用通道/严重度被过滤）时不写去重标记，
// 下一次 tick 会重试，避免整期报告静默丢失。
func (r *Runner) sendReport(cfg *notify.Config, kind string, now time.Time) bool {
	views := r.st.Nodes()
	online, stale, offline, noData := 0, 0, 0, 0
	var cpuSum, memSum float64
	var cost float64
	var rx, tx uint64
	var expiring []string
	for _, n := range views {
		m, ts, _ := r.st.Latest(n.ID)
		switch store.NodeState(ts, now.Unix()) {
		case model.NodeStateOnline:
			online++
			cpuSum += m.CPUPct
			memSum += pct(m.MemUsed, m.MemTotal)
			rx += m.NetRx
			tx += m.NetTx
		case model.NodeStateStale:
			stale++
		case model.NodeStateOffline:
			offline++
		default:
			noData++
		}
		cost += monthly(n.Price, n.Currency, r.cfg.CNYRate)
		if days, ok := daysUntil(n.ExpireAt, now); ok && days >= 0 && days <= 30 {
			expiring = append(expiring, fmt.Sprintf("%s（%d 天）", n.Name, days))
		}
	}
	svcs := r.st.Services()
	down := 0
	for _, sv := range svcs {
		if _, _, ok, has := r.st.ServiceLast(sv.ID); has && !ok {
			down++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "节点：%d 台（在线 %d / 延迟 %d / 离线 %d / 未上报 %d）\n", len(views), online, stale, offline, noData)
	if online > 0 {
		fmt.Fprintf(&b, "平均 CPU：%.1f%%，平均内存：%.1f%%\n", cpuSum/float64(online), memSum/float64(online))
	}
	fmt.Fprintf(&b, "活跃告警：%d 条\n", r.st.ActiveAlertCount())
	fmt.Fprintf(&b, "服务探测：%d 个，异常 %d 个\n", len(svcs), down)
	fmt.Fprintf(&b, "累计流量：↑ %.2f GB / ↓ %.2f GB\n", float64(rx)/(1<<30), float64(tx)/(1<<30))
	fmt.Fprintf(&b, "月度成本：$%.2f\n", cost)
	if len(expiring) > 0 {
		fmt.Fprintf(&b, "30 天内到期：%s\n", strings.Join(expiring, "、"))
	}
	title := "每日运行报告"
	switch kind {
	case "weekly":
		title = "每周运行报告"
	case "monthly":
		title = "每月运行报告"
	}
	if !r.ob.Enqueue(notify.Message{
		Title:    title,
		Body:     strings.TrimRight(b.String(), "\n"),
		Severity: "warn",
		RuleID:   "report." + kind,
		RuleName: "运行报告",
		TS:       now.Unix(),
		Link:     r.cfg.PanelLink,
	}) {
		return false
	}
	log.Printf("已生成%s", title)
	return true
}

func pct(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) / float64(total) * 100
}

func monthly(price float64, currency string, rate float64) float64 {
	if strings.EqualFold(currency, "CNY") {
		return price / rate
	}
	return price
}

func daysUntil(date string, now time.Time) (int, bool) {
	t, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(date), now.Location())
	if err != nil {
		return 0, false
	}
	// 按日历日计算：直接对本地零点求差，夏令时切换日只有 23/25 小时，
	// 截断后会把「还有 2 天」算成 1 天。用 UTC 的日期算术，差值恒为 24h 的整数倍。
	ty, tm, td := t.Date()
	ny, nm, nd := now.Date()
	target := time.Date(ty, tm, td, 0, 0, 0, 0, time.UTC)
	today := time.Date(ny, nm, nd, 0, 0, 0, 0, time.UTC)
	return int(target.Sub(today).Hours() / 24), true
}

// Package schedule 运行与告警引擎无关的定时任务：
// 资产到期提醒与每日/每周运行报告。
package schedule

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/motao/rollcall/internal/model"
	"github.com/motao/rollcall/internal/notify"
	"github.com/motao/rollcall/internal/outbox"
	"github.com/motao/rollcall/internal/store"
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
		return nil
	}
	return cfg
}

func (r *Runner) expiryTick() {
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
		if !r.st.MarkExpirySent(n.ID, day) {
			continue
		}
		level := "warn"
		title := "资产即将到期"
		if days == 0 {
			title = "资产今天到期"
			level = "critical"
		}
		body := fmt.Sprintf("到期日期：%s，剩余 %d 天。请及时续费，避免服务中断。", n.ExpireAt, days)
		r.ob.Enqueue(notify.Message{
			Title:    title,
			Body:     body,
			Severity: level,
			NodeName: n.Name,
			NodeID:   n.ID,
			RuleID:   "expiry",
			RuleName: "资产到期提醒",
			TS:       now.Unix(),
			Link:     r.cfg.PanelLink,
		})
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
		if r.st.GetSetting(key) == "" {
			r.sendReport(cfg, "daily", now)
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
		if r.st.GetSetting(key) == "" {
			r.sendReport(cfg, "weekly", now)
			r.st.SetSetting(key, "1")
		}
	}
	// 月报在每月 1 日发送；与日报同一时刻时两条独立入队。
	if cfg.ReportMonth && now.Day() == 1 && now.Hour() >= cfg.ReportHour {
		key := "report.monthly." + now.Format("2006-01")
		if r.st.GetSetting(key) == "" {
			r.sendReport(cfg, "monthly", now)
			r.st.SetSetting(key, "1")
		}
	}
}

func (r *Runner) sendReport(cfg *notify.Config, kind string, now time.Time) {
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
	r.ob.Enqueue(notify.Message{
		Title:    title,
		Body:     strings.TrimRight(b.String(), "\n"),
		Severity: "warn",
		RuleID:   "report." + kind,
		RuleName: "运行报告",
		TS:       now.Unix(),
		Link:     r.cfg.PanelLink,
	})
	log.Printf("已生成%s", title)
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
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return int(t.Sub(today).Hours() / 24), true
}

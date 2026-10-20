package schedule

import (
	"fmt"
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/notify"
	"github.com/motao123/rollcall/internal/outbox"
	"github.com/motao123/rollcall/internal/store"
)

// testWebhookChannel 让通知配置拥有一个可用通道：
// Enqueue 在没有可用通道时会返回 false，报告/提醒不会写去重标记。
const testWebhookChannel = `"channels":[{"id":"c1","type":"webhook","name":"test","enabled":true,"options":{"url":"http://127.0.0.1/hook"}}]`

// newTestRunner 构造与生产一致的 Runner：Outbox 必须先拿到通知配置，
// 否则 Enqueue 会把所有消息当成"通知已停用"直接丢弃。
func newTestRunner(t *testing.T, st *store.Store) (*Runner, *outbox.Manager) {
	t.Helper()
	ob := outbox.New(st, outbox.DefaultConfig())
	syncNotify(t, st, ob)
	return New(st, ob, Config{}), ob
}

func syncNotify(t *testing.T, st *store.Store, ob *outbox.Manager) {
	t.Helper()
	cfg, err := notify.Parse(st.GetSetting("notify"))
	if err != nil {
		t.Fatalf("解析通知配置: %v", err)
	}
	ob.SetConfig(cfg)
}

func TestDaysUntil(t *testing.T) {
	now := time.Date(2026, 3, 1, 15, 30, 0, 0, time.Local)
	cases := []struct {
		date string
		want int
		ok   bool
	}{
		{"2026-03-01", 0, true},
		{"2026-03-02", 1, true},
		{"2026-02-28", -1, true},
		{"2026-04-01", 31, true},
		{"", 0, false},
		{"2026-13-01", 0, false},
		{"03/02/2026", 0, false},
	}
	for _, c := range cases {
		got, ok := daysUntil(c.date, now)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("daysUntil(%q)=%d,%v; want %d,%v", c.date, got, ok, c.want, c.ok)
		}
	}
}

func TestMonthlyCost(t *testing.T) {
	if got := monthly(120, "USD", 7.2); got != 120 {
		t.Errorf("USD 不应折算: %v", got)
	}
	if got := monthly(720, "CNY", 7.2); got != 100 {
		t.Errorf("CNY 应折算为 USD: %v", got)
	}
	if got := monthly(72, "cny", 7.2); got != 10 {
		t.Errorf("币种应大小写不敏感: %v", got)
	}
}

// TestExpiryTickDedupesPerDay 验证到期提醒每天每节点只入队一次，
// 且无效到期日不会产生事件。
func TestExpiryTickDedupesPerDay(t *testing.T) {
	st := store.New(t.TempDir())
	exp := time.Now().AddDate(0, 0, 3).Format("2006-01-02")
	st.UpsertNode(&model.Node{ID: "n1", Name: "节点一", ExpireAt: exp})
	st.UpsertNode(&model.Node{ID: "bad", Name: "坏日期", ExpireAt: "not-a-date"})
	st.SetSetting("notify", `{"enabled":true,"expiryDays":7,`+testWebhookChannel+`}`)

	r, _ := newTestRunner(t, st)
	countExpiry := func() int {
		n := 0
		for _, e := range st.Events(0) {
			if e.Kind == "expiry" {
				n++
			}
		}
		return n
	}
	r.expiryTick()
	if got := countExpiry(); got != 1 {
		t.Fatalf("应产生 1 条到期事件，实际 %d", got)
	}
	r.expiryTick()
	if got := countExpiry(); got != 1 {
		t.Errorf("同一天重复检查不应重复提醒，实际 %d 条", got)
	}
	if evs := st.Events(0); evs[0].Kind != "expiry" || evs[0].NodeID != "n1" {
		t.Errorf("事件内容错误: %+v", evs[0])
	}
}

func TestReportTickWritesDedupeMarkers(t *testing.T) {
	st := store.New(t.TempDir())
	st.SetSetting("notify", `{"enabled":true,"reportDaily":true,"reportWeekly":true,"reportHour":9,`+testWebhookChannel+`}`)
	r, _ := newTestRunner(t, st)

	now := time.Date(2026, 10, 19, 9, 30, 0, 0, time.Local) // 周一
	if now.Weekday() != time.Monday {
		t.Fatalf("测试日期应为周一，实际 %s", now.Weekday())
	}
	r.reportTick(now)
	if st.GetSetting("report.daily.2026-10-19") == "" {
		t.Error("日报标记未写入")
	}
	_, week := now.ISOWeek()
	if st.GetSetting(fmt.Sprintf("report.weekly.%d-W%02d", now.Year(), week)) == "" {
		t.Error("周报标记未写入")
	}
	// 非整点不重复发送（标记保持即可，验证不会报错）。
	r.reportTick(now.Add(30 * time.Minute))
	if st.GetSetting("report.daily.2026-10-19") == "" {
		t.Error("日报标记不应被清除")
	}

	monthlyStore := store.New(t.TempDir())
	monthlyStore.SetSetting("notify", `{"enabled":true,"reportMonthly":true,"reportHour":9,`+testWebhookChannel+`}`)
	rm, _ := newTestRunner(t, monthlyStore)
	first := time.Date(2026, 11, 1, 9, 5, 0, 0, time.Local)
	rm.reportTick(first)
	if monthlyStore.GetSetting("report.monthly.2026-11") == "" {
		t.Error("月报标记未写入")
	}
}

// TestWeeklyReportKeyUsesISOYear 是回归：周报去重键必须用 ISO 年。
//
// 2024-12-30 是周一，属于 ISO 2025-W01，但日历年是 2024。
// 用 now.Year() 会拼出 report.weekly.2024-W01，与 2024-01-01 那周的键相同，
// 当周周报被判为"已发送"而静默跳过。
func TestWeeklyReportKeyUsesISOYear(t *testing.T) {
	st := store.New(t.TempDir())
	st.SetSetting("notify", `{"enabled":true,"reportWeekly":true,"reportHour":9,`+testWebhookChannel+`}`)
	r, _ := newTestRunner(t, st)

	now := time.Date(2024, 12, 30, 9, 30, 0, 0, time.Local) // 周一，ISO 2025-W01
	if now.Weekday() != time.Monday {
		t.Fatalf("测试日期应为周一，实际 %s", now.Weekday())
	}
	isoYear, week := now.ISOWeek()
	if isoYear == now.Year() {
		t.Fatalf("测试日期应跨 ISO 年（ISO %d vs 日历年 %d）", isoYear, now.Year())
	}
	r.reportTick(now)

	want := fmt.Sprintf("report.weekly.%d-W%02d", isoYear, week)
	if st.GetSetting(want) == "" {
		t.Errorf("缺少以 ISO 年构造的去重键 %q", want)
	}
	wrong := fmt.Sprintf("report.weekly.%d-W%02d", now.Year(), week)
	if st.GetSetting(wrong) != "" {
		t.Errorf("不应写入按日历年构造的键 %q（会与年初那周碰撞）", wrong)
	}
}

// TestReportCatchUpAfterScheduledHour 是回归：
// 进程可能在报告时刻没在运行（重启/升级/时钟跳变），
// 用 Hour()== 判断会让整期报告静默丢失，与"当天内仍可补发"的约定不符。
func TestReportCatchUpAfterScheduledHour(t *testing.T) {
	st := store.New(t.TempDir())
	st.SetSetting("notify", `{"enabled":true,"reportDaily":true,"reportMonthly":true,"reportHour":9,`+testWebhookChannel+`}`)
	r, _ := newTestRunner(t, st)

	// 11:30 才启动：日报应补发（当天只发一次）
	late := time.Date(2026, 11, 2, 11, 30, 0, 0, time.Local)
	r.reportTick(late)
	if st.GetSetting("report.daily.2026-11-02") == "" {
		t.Error("错过报告时刻后当天内应补发日报")
	}

	// 同一天再次触发不应重复（去重键生效）
	before := len(st.Events(0))
	r.reportTick(late.Add(time.Minute))
	if got := len(st.Events(0)); got != before {
		t.Errorf("同一天不应重复发送，事件数 %d → %d", before, got)
	}

	// 月报同理：1 日 11:30 触发仍应发送
	monthly := time.Date(2026, 12, 1, 11, 30, 0, 0, time.Local)
	r.reportTick(monthly)
	if st.GetSetting("report.monthly.2026-12") == "" {
		t.Error("错过报告时刻后当月 1 日内应补发月报")
	}
}

// TestDaysUntilAcrossDST 回归：夏令时切换日的本地零点相差 23/25 小时，
// 直接除以 24 截断会把「还有 2 天」算成 1 天。
func TestDaysUntilAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("缺少时区数据: %v", err)
	}
	// 2026-03-08 春季前拨，当地这一天只有 23 小时。
	now := time.Date(2026, 3, 7, 12, 0, 0, 0, loc)
	if got, ok := daysUntil("2026-03-09", now); !ok || got != 2 {
		t.Fatalf("跨春季切换应为 2 天，实际 %d,%v", got, ok)
	}
	if got, ok := daysUntil("2026-03-08", now); !ok || got != 1 {
		t.Fatalf("次日应为 1 天，实际 %d,%v", got, ok)
	}
	// 2026-11-01 秋季回拨，当地这一天有 25 小时。
	now = time.Date(2026, 10, 31, 12, 0, 0, 0, loc)
	if got, ok := daysUntil("2026-11-02", now); !ok || got != 2 {
		t.Fatalf("跨秋季切换应为 2 天，实际 %d,%v", got, ok)
	}
}

// TestReportRetriesUntilEnqueueSucceeds 回归：入队失败时不能写去重标记，
// 否则队列满或没有可用通道会让整期报告静默丢失。
func TestReportRetriesUntilEnqueueSucceeds(t *testing.T) {
	st := store.New(t.TempDir())
	st.SetSetting("notify", `{"enabled":true,"channels":[],"reportDaily":true,"reportHour":9}`)
	r, ob := newTestRunner(t, st)
	now := time.Date(2026, 11, 3, 9, 30, 0, 0, time.Local) // 周二

	r.reportTick(now)
	if st.GetSetting("report.daily.2026-11-03") != "" {
		t.Fatal("没有可用通道时不应写日报去重标记")
	}
	if got := st.NotifyStats().Pending; got != 0 {
		t.Fatalf("没有可用通道时不应入队，实际 %d", got)
	}

	st.SetSetting("notify", `{"enabled":true,"reportDaily":true,"reportHour":9,`+testWebhookChannel+`}`)
	syncNotify(t, st, ob)
	r.reportTick(now)
	if st.GetSetting("report.daily.2026-11-03") == "" {
		t.Fatal("通道恢复后应补发日报并写入标记")
	}
	if got := st.NotifyStats().Pending; got != 1 {
		t.Fatalf("补发的日报应进入队列，实际 %d", got)
	}
}

// TestExpiryRetriesUntilEnqueueSucceeds 回归：到期提醒同样只在真正入队后
// 才记「今天已提醒」并写入活动流，通道恢复后可以补发。
func TestExpiryRetriesUntilEnqueueSucceeds(t *testing.T) {
	st := store.New(t.TempDir())
	now := time.Now()
	st.UpsertNode(&model.Node{ID: "n1", Name: "节点一", ExpireAt: now.AddDate(0, 0, 3).Format("2006-01-02")})
	st.SetSetting("notify", `{"enabled":true,"expiryDays":7,"channels":[]}`)
	r, ob := newTestRunner(t, st)
	day := now.Format("2006-01-02")
	countExpiry := func() int {
		n := 0
		for _, e := range st.Events(0) {
			if e.Kind == "expiry" {
				n++
			}
		}
		return n
	}

	r.expiryTick()
	if st.ExpirySentFor("n1", day) {
		t.Fatal("未入队时不应标记为已提醒")
	}
	if got := countExpiry(); got != 0 {
		t.Fatalf("未入队时不应写活动事件，实际 %d", got)
	}

	st.SetSetting("notify", `{"enabled":true,"expiryDays":7,`+testWebhookChannel+`}`)
	syncNotify(t, st, ob)
	r.expiryTick()
	if !st.ExpirySentFor("n1", day) {
		t.Fatal("入队成功后应标记为已提醒")
	}
	if got := countExpiry(); got != 1 {
		t.Fatalf("入队成功后应写 1 条活动事件，实际 %d", got)
	}
	r.expiryTick()
	if got := countExpiry(); got != 1 {
		t.Fatalf("同一天不应重复提醒，实际 %d 条", got)
	}
}

// TestPruneReportMarkers 回归：
// 去重键每期一个且永不过期，长期运行会让 settings 无界增长。
// 清理只应删除过期的 report.* 键，不能碰其它设置，也不能误删格式异常的键。
func TestPruneReportMarkers(t *testing.T) {
	st := store.New(t.TempDir())
	now := time.Date(2026, 10, 19, 10, 0, 0, 0, time.Local)

	stale := []string{
		"report.daily.2025-01-01",
		"report.weekly.2023-W05",
		"report.monthly.2023-01",
	}
	fresh := []string{
		"report.daily." + now.Format("2006-01-02"),
		"report.weekly.2026-W42",
		"report.monthly.2026-10",
	}
	for _, k := range append(append([]string{}, stale...), fresh...) {
		st.SetSetting(k, "1")
	}
	st.SetSetting("theme", `{"mode":"dark"}`)
	st.SetSetting("report.weekly.garbage", "1") // 格式异常：保持原样，不误删

	r, _ := newTestRunner(t, st)
	r.pruneReportMarkers(now)

	for _, k := range stale {
		if st.GetSetting(k) != "" {
			t.Errorf("过期标记未清理: %s", k)
		}
	}
	for _, k := range fresh {
		if st.GetSetting(k) == "" {
			t.Errorf("当期标记被误删: %s", k)
		}
	}
	if st.GetSetting("theme") == "" {
		t.Error("非 report 前缀的设置被误删")
	}
	if st.GetSetting("report.weekly.garbage") == "" {
		t.Error("格式异常的键应原样保留")
	}

	// 接线验证：expiryTick 必须真的调用清理（否则清理永远不会发生）
	wired := store.New(t.TempDir())
	wired.SetSetting("report.daily.2000-01-01", "1")
	wired.SetSetting("notify", `{"enabled":false}`)
	r, _ = newTestRunner(t, wired)
	r.expiryTick()
	if wired.GetSetting("report.daily.2000-01-01") != "" {
		t.Error("expiryTick 未触发报告标记清理")
	}
}

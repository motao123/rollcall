package schedule

import (
	"fmt"
	"testing"
	"time"

	"github.com/motao/rollcall/internal/model"
	"github.com/motao/rollcall/internal/outbox"
	"github.com/motao/rollcall/internal/store"
)

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
	st.SetSetting("notify", `{"enabled":true,"expiryDays":7}`)

	r := New(st, outbox.New(st, outbox.DefaultConfig()), Config{})
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
	st.SetSetting("notify", `{"enabled":true,"channels":[],"reportDaily":true,"reportWeekly":true,"reportHour":9}`)
	r := New(st, outbox.New(st, outbox.DefaultConfig()), Config{})

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
	monthlyStore.SetSetting("notify", `{"enabled":true,"channels":[],"reportMonthly":true,"reportHour":9}`)
	rm := New(monthlyStore, outbox.New(monthlyStore, outbox.DefaultConfig()), Config{})
	first := time.Date(2026, 11, 1, 9, 5, 0, 0, time.Local)
	rm.reportTick(first)
	if monthlyStore.GetSetting("report.monthly.2026-11") == "" {
		t.Error("月报标记未写入")
	}
}

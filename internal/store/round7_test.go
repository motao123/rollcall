package store

import (
	"strings"
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/model"
)

func TestTrafficCycleAccumulatesAndResets(t *testing.T) {
	st := New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "n1", Token: "t", TrafficResetDay: 1})
	base := time.Date(2026, 3, 15, 12, 0, 0, 0, time.Local).Unix()
	add := func(ts int64, rx, tx uint64) {
		st.AddMetrics("n1", model.HostInfo{}, model.Metrics{TS: ts, NetRx: rx, NetTx: tx})
	}

	add(base, 1000, 2000)
	u := st.TrafficUsage("n1")
	if u.UsedRx != 0 || u.UsedTx != 0 {
		t.Fatalf("首个样本只建立基线，不应计入用量: %+v", u)
	}
	if want := time.Date(2026, 3, 1, 0, 0, 0, 0, time.Local).Unix(); u.CycleStart != want {
		t.Errorf("周期起点错误: got %d want %d", u.CycleStart, want)
	}

	add(base+60, 1500, 2500)
	u = st.TrafficUsage("n1")
	if u.UsedRx != 500 || u.UsedTx != 500 {
		t.Fatalf("增量累计错误: %+v", u)
	}

	// 只有下行计数器归零时，上行增量不能丢：两个方向独立判断。
	add(base+120, 100, 3000)
	u = st.TrafficUsage("n1")
	if u.UsedRx != 600 || u.UsedTx != 1000 {
		t.Fatalf("单方向回绕处理错误: %+v", u)
	}

	// 跨过重置日：进入新周期，用量归零并从当前计数重新建立基线。
	add(time.Date(2026, 4, 2, 12, 0, 0, 0, time.Local).Unix(), 5000, 9000)
	u = st.TrafficUsage("n1")
	if u.UsedRx != 0 || u.UsedTx != 0 {
		t.Fatalf("新周期应从零开始: %+v", u)
	}
	if want := time.Date(2026, 4, 1, 0, 0, 0, 0, time.Local).Unix(); u.CycleStart != want {
		t.Errorf("新周期起点错误: got %d want %d", u.CycleStart, want)
	}
	add(time.Date(2026, 4, 2, 12, 1, 0, 0, time.Local).Unix(), 5100, 9100)
	u = st.TrafficUsage("n1")
	if u.UsedRx != 100 || u.UsedTx != 100 {
		t.Errorf("新周期累计错误: %+v", u)
	}

	// 双向归零（Agent 重启 / 网卡重载）：只重建基线，**保留本周期已用量**。
	// 清零会把"这个月已经跑掉的流量"抹掉，配额告警随之偏低；
	// 而重置基线已经避免了把上次开机的计数重复加进来。
	add(time.Date(2026, 4, 2, 12, 2, 0, 0, time.Local).Unix(), 10, 20)
	u = st.TrafficUsage("n1")
	if u.UsedRx != 100 || u.UsedTx != 100 {
		t.Errorf("计数器归零应保留已用量、只重置基线: %+v", u)
	}
	// 归零后继续正常累计：基线是 10/20
	add(time.Date(2026, 4, 2, 12, 3, 0, 0, time.Local).Unix(), 60, 120)
	u = st.TrafficUsage("n1")
	if u.UsedRx != 150 || u.UsedTx != 200 {
		t.Errorf("归零后累计错误: %+v", u)
	}
}

func TestTrafficCycleStartUsesResetDay(t *testing.T) {
	loc := time.Local
	in := time.Date(2026, 4, 10, 12, 0, 0, 0, loc)
	got := time.Unix(trafficCycleStart(in, 15), 0).In(loc)
	if want := time.Date(2026, 3, 15, 0, 0, 0, 0, loc); !got.Equal(want) {
		t.Errorf("10 日应归属上月 15 日周期: got %s want %s", got, want)
	}
	in = time.Date(2026, 4, 20, 12, 0, 0, 0, loc)
	got = time.Unix(trafficCycleStart(in, 15), 0).In(loc)
	if want := time.Date(2026, 4, 15, 0, 0, 0, 0, loc); !got.Equal(want) {
		t.Errorf("20 日应归属本月 15 日周期: got %s want %s", got, want)
	}
	// 非法重置日按 1 号处理。
	in = time.Date(2026, 4, 20, 12, 0, 0, 0, loc)
	got = time.Unix(trafficCycleStart(in, 0), 0).In(loc)
	if want := time.Date(2026, 4, 1, 0, 0, 0, 0, loc); !got.Equal(want) {
		t.Errorf("非法重置日应按 1 号: got %s want %s", got, want)
	}
}

func TestEventsCappedAndNewestFirst(t *testing.T) {
	st := New(t.TempDir())
	for i := 0; i < maxEvents+5; i++ {
		st.AddEvent(model.Event{Kind: "node_online", Text: "e" + itoa(i), TS: int64(i + 1)})
	}
	all := st.Events(0)
	if len(all) != maxEvents {
		t.Fatalf("事件数应封顶 %d，实际 %d", maxEvents, len(all))
	}
	if all[0].Text != "e204" || all[len(all)-1].Text != "e5" {
		t.Errorf("事件顺序或淘汰策略错误: first=%s last=%s", all[0].Text, all[len(all)-1].Text)
	}
	if got := st.Events(10); len(got) != 10 || got[0].Text != "e204" {
		t.Errorf("limit 未生效: len=%d", len(got))
	}
	// 空文本事件直接忽略。
	st.AddEvent(model.Event{Kind: "x"})
	if got := st.Events(0); len(got) != maxEvents {
		t.Errorf("空事件不应写入: %d", len(got))
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [8]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}

func TestAPITokenLifecycle(t *testing.T) {
	st := New(t.TempDir())
	now := time.Now().Unix()
	tok := &model.APIToken{
		ID: "tk1", Name: "ci", Prefix: "rct_abcdefgh", Hash: "hash-1",
		Scopes: []string{"read"}, Paths: []string{"/api/overview"}, CreatedAt: now,
	}
	st.AddAPIToken(tok)
	tok.Scopes[0] = "admin" // AddAPIToken 必须复制切片，不能共享底层数组
	tok.Paths[0] = "/api/admin"

	list := st.APITokens()
	if len(list) != 1 || list[0].Hash != "" || len(list[0].Scopes) != 1 || list[0].Scopes[0] != "read" ||
		len(list[0].Paths) != 1 || list[0].Paths[0] != "/api/overview" {
		t.Fatalf("令牌列表不应泄露摘要或受入参影响: %+v", list)
	}

	got, ok := st.MatchAPIToken("hash-1", now)
	if !ok || got.ID != "tk1" {
		t.Fatalf("有效令牌应匹配: %+v ok=%v", got, ok)
	}
	if !got.HasScope("read") || got.HasScope("admin") {
		t.Errorf("read 令牌的作用域判断错误: %v", got.Scopes)
	}
	if got.LastUsedAt != now {
		t.Errorf("最近使用时间未更新: %d", got.LastUsedAt)
	}
	if _, ok := st.MatchAPIToken("wrong", now); ok {
		t.Error("错误摘要不应匹配")
	}

	// 过期令牌立即失效。
	st.AddAPIToken(&model.APIToken{
		ID: "tk2", Hash: "hash-2", Scopes: []string{"admin"},
		CreatedAt: now, ExpiresAt: now - 1,
	})
	if _, ok := st.MatchAPIToken("hash-2", now); ok {
		t.Error("过期令牌不应匹配")
	}

	// admin 作用域隐含 read。
	adminTok := &model.APIToken{Scopes: []string{"admin"}}
	if !adminTok.HasScope("read") || !adminTok.HasScope("admin") {
		t.Error("admin 应隐含 read")
	}

	if !st.DeleteAPIToken("tk1") || st.DeleteAPIToken("tk1") {
		t.Error("删除语义应为首次成功、再次失败")
	}
	if _, ok := st.MatchAPIToken("hash-1", now); ok {
		t.Error("删除后令牌不应再匹配")
	}
}

func TestExpiryReminderOncePerDay(t *testing.T) {
	st := New(t.TempDir())
	if !st.MarkExpirySent("n1", "2026-10-19") {
		t.Fatal("首次标记应成功")
	}
	if st.MarkExpirySent("n1", "2026-10-19") {
		t.Fatal("同一天重复标记应返回 false")
	}
	if !st.MarkExpirySent("n1", "2026-10-20") {
		t.Fatal("次日应重新允许提醒")
	}
	if !st.MarkExpirySent("n2", "2026-10-19") {
		t.Fatal("不同节点互不影响")
	}
}

func TestRenewalsSortAndDelete(t *testing.T) {
	st := New(t.TempDir())
	st.AddRenewal(&model.Renewal{ID: "r1", NodeID: "n1", At: "2026-01-05", Amount: 10})
	st.AddRenewal(&model.Renewal{ID: "r2", NodeID: "n1", At: "2026-03-05", Amount: 20})
	st.AddRenewal(&model.Renewal{ID: "r3", NodeID: "n2", At: "2026-02-05", Amount: 30})

	rows := st.Renewals("n1")
	if len(rows) != 2 || rows[0].ID != "r2" || rows[1].ID != "r1" {
		t.Fatalf("应按日期倒序且按节点过滤: %+v", rows)
	}
	if !st.DeleteRenewal("r2") || st.DeleteRenewal("r2") {
		t.Error("删除语义应为首次成功、再次失败")
	}
	if rows := st.Renewals("n1"); len(rows) != 1 {
		t.Errorf("删除后应剩 1 条: %+v", rows)
	}
}

func TestConfigExportImportPreservesTokens(t *testing.T) {
	st := New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "节点一", Token: "report-token-abc"})
	st.UpsertService(&model.Service{ID: "s1", NodeID: "n1", Name: "Web", Type: "http", Target: "http://127.0.0.1", Enabled: true})
	st.UpsertRule(&model.AlertRule{ID: "r1", Name: "CPU", Metric: "cpu", Op: "gt", Threshold: 90, Enabled: true})
	st.SetSilence(&model.Silence{Scope: "r1", Until: time.Now().Add(time.Hour).Unix(), Reason: "维护"})
	st.AddRenewal(&model.Renewal{ID: "rn1", NodeID: "n1", At: "2026-03-01", Amount: 10, Currency: "USD"})
	st.SetSetting("notify", `{"enabled":true}`)
	st.SetSetting("revoked_sessions", `{"session":"local"}`)

	raw, err := st.ExportConfig()
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	if !strings.Contains(string(raw), "report-token-abc") {
		t.Fatal("备份应包含独立的节点令牌表")
	}
	if strings.Contains(string(raw), `"session":"local"`) || strings.Contains(string(raw), "revoked_sessions") {
		t.Fatal("会话吊销列表属于运行态安全状态，不应出现在备份中")
	}

	st2 := New(t.TempDir())
	st2.SetSetting("revoked_sessions", `{"session":"keep-me"}`)
	st2.SetSetting("theme", `{"mode":"dark"}`) // 本地已有、备份中没有的设置应保留
	// 导入前的僵尸告警：引用的规则/节点在新配置中不存在，导入后必须关闭。
	st2.UpsertNode(&model.Node{ID: "old", Name: "旧节点", Token: "t"})
	st2.UpsertRule(&model.AlertRule{ID: "r_old", Name: "旧规则", Metric: "cpu", Op: "gt", Threshold: 1, Enabled: true})
	if created, ok := st2.OpenAlert(&model.Alert{RuleID: "r_old", NodeID: "old", Severity: "warn", StartedAt: 1}); !created || !ok {
		t.Fatalf("构造前置告警失败: created=%v ok=%v", created, ok)
	}

	if err := st2.ImportConfig(raw); err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	n, ok := st2.Node("n1")
	if !ok || n.Token != "report-token-abc" {
		t.Fatalf("导入后节点令牌丢失: %+v", n)
	}
	if _, ok := st2.NodeByToken("report-token-abc", "n1"); !ok {
		t.Fatal("导入后令牌应能通过 Agent 认证")
	}
	if _, ok := st2.Node("old"); ok {
		t.Error("旧节点应被替换")
	}
	if len(st2.Services()) != 1 || len(st2.Rules()) != 1 || len(st2.Renewals("n1")) != 1 {
		t.Errorf("服务/规则/续费未完整导入")
	}
	if scope, _, ok := st2.SilenceFor("r1", time.Now().Unix()); !ok || scope != "r1" {
		t.Error("静默期未导入")
	}
	if got := st2.GetSetting("notify"); got != `{"enabled":true}` {
		t.Errorf("通知设置未导入: %q", got)
	}
	if got := st2.GetSetting("revoked_sessions"); got != `{"session":"keep-me"}` {
		t.Errorf("会话吊销列表必须保留本机值: %q", got)
	}
	if got := st2.GetSetting("theme"); got != `{"mode":"dark"}` {
		t.Errorf("备份中没有的本地设置应保留: %q", got)
	}
	if got := st2.ActiveAlertCount(); got != 0 {
		t.Errorf("引用已删除规则的告警应被关闭，实际 %d 条活跃", got)
	}

	// 非法/未来版本与未知字段必须整体拒绝，不能部分导入。
	if err := st2.ImportConfig([]byte(`{"version":99,"nodes":[],"services":[],"rules":[]}`)); err == nil {
		t.Error("未来版本应被拒绝")
	}
	if err := st2.ImportConfig([]byte(`{"version":2,"nodes":[],"services":[],"rules":[],"oops":1}`)); err == nil {
		t.Error("未知字段应被拒绝")
	}
}

// v1 备份没有令牌表：导入时必须生成新令牌，而不是让 Agent 带着空令牌启动。
func TestConfigImportV1GeneratesToken(t *testing.T) {
	st := New(t.TempDir())
	raw := []byte(`{"version":1,"nodes":[{"id":"n1","name":"旧备份节点"}],"services":[],"rules":[]}`)
	if err := st.ImportConfig(raw); err != nil {
		t.Fatalf("v1 备份应可导入: %v", err)
	}
	n, ok := st.Node("n1")
	if !ok || n.Token == "" {
		t.Fatalf("导入 v1 备份后应生成新令牌: %+v", n)
	}
}

package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/model"
)

// TestSaveRacesWithWrites 是回归测试：Save() 必须在持锁期间完成深拷贝。
//
// 曾经的缺陷：快照里直接引用 s.nodes / s.hosts / s.settings 等 map，
// 解锁后才 json.Marshal，marshal 迭代 map 时与写入 goroutine 撞车，
// 触发不可 recover 的 "fatal error: concurrent map iteration and map write"，
// 整个进程死亡。StartAutoSave 每 60 秒触发一次，因此这是必然事件。
//
// 注意：Go runtime 的 map 并发检测**不依赖 -race**，
// 因此在没有 gcc / CGO 的机器上同样能捕获此缺陷。
func TestSaveRacesWithWrites(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)

	for i := 0; i < 40; i++ {
		s.UpsertNode(&model.Node{ID: NewID("n"), Name: "node", Token: NewToken()})
	}
	for _, sv := range []*model.Service{{ID: NewID("s"), Name: "svc", Type: "tcp", Target: "1.2.3.4:80"}} {
		s.UpsertService(sv)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ruleIDs := []string{"r_a", "r_b", "r_c"}
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			nodes := s.Nodes()
			if len(nodes) > 0 {
				n := nodes[i%len(nodes)]
				// 写 hosts map —— 与 marshal 迭代直接冲突
				s.AddMetrics(n.ID, model.HostInfo{Hostname: "h"}, model.Metrics{TS: time.Now().UnixNano()})
			}
			s.SetSetting("notify", `{"enabled":true}`)
			s.UpsertRule(&model.AlertRule{ID: ruleIDs[i%len(ruleIDs)], Name: "r", Enabled: true})
			// 让出调度：真实场景是 30s 一次上报，紧循环会饿死 Save 的 RLock
			if i%64 == 63 {
				time.Sleep(100 * time.Microsecond)
			}
		}
	}()

	// -race 插桩会把 300 次全量快照放大到十分钟以上；50 次已足够与写入并发。
	saves := 300
	if raceEnabled {
		saves = 50
	}
	for i := 0; i < saves; i++ {
		if err := s.Save(); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("Save 返回错误: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}

// TestSaveStats 校验落盘计数与耗时统计，失败时只累计失败次数。
func TestSaveStats(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	st := s.SaveStats()
	if st.Count != 1 || st.Failures != 0 {
		t.Fatalf("首次落盘统计异常: %+v", st)
	}
	if st.LastSuccess == 0 || st.LastDurationSeconds < 0 {
		t.Fatalf("首次落盘缺少耗时/时间戳: %+v", st)
	}

	// 把 state.json 换成目录，使 rename 必然失败（root 也无法覆盖目录）
	if err := os.Remove(filepath.Join(dir, "state.json")); err != nil {
		t.Fatalf("移除快照: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "state.json"), 0o750); err != nil {
		t.Fatalf("创建目录: %v", err)
	}
	if err := s.Save(); err == nil {
		t.Fatal("目标为目录时 Save 应失败")
	}
	st = s.SaveStats()
	if st.Count != 2 || st.Failures != 1 {
		t.Fatalf("失败后统计异常: %+v", st)
	}
}

// TestSavePersistsAllSections 确保深拷贝重构没有丢字段。
func TestSavePersistsAllSections(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)

	n := &model.Node{ID: "n1", Name: "节点一", Provider: "test", Token: "tok-secret"}
	s.UpsertNode(n)
	s.AddMetrics("n1", model.HostInfo{Hostname: "h1", OS: "Debian 12"}, model.Metrics{TS: 1000, CPUPct: 42.5})
	s.UpsertService(&model.Service{ID: "s1", Name: "svc", Type: "tcp", Target: "1.2.3.4:80", Interval: 60, Enabled: true})
	s.AddServiceResult(model.ServiceResult{ServiceID: "s1", TS: 1000, OK: true, LatencyMs: 12})
	s.UpsertRule(&model.AlertRule{ID: "r1", Name: "规则一", Metric: "cpu", Op: "gt", Threshold: 90, Enabled: true})
	s.OpenAlert(&model.Alert{ID: "a1", RuleID: "r1", NodeID: "n1", Severity: "warn", StartedAt: 1000})
	s.SetSetting("notify", `{"enabled":true}`)
	// APIToken.Hash 也是 json:"-"，必须像节点令牌一样单独持久化，
	// 否则重启后 scoped 令牌全部变成永远 401 的幽灵记录。
	s.AddAPIToken(&model.APIToken{ID: "tk1", Name: "自动化", Hash: "hash-secret",
		Scopes: []string{"read"}, CreatedAt: 1000})

	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	b := readSnapshotFile(t, filepath.Join(dir, "state.json"))
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("解析快照: %v", err)
	}

	for _, key := range []string{"nodes", "hosts", "series", "services", "svcRes", "rules", "alerts", "tokens", "settings", "apiTokens", "apiTokenHashes"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("快照缺少字段 %q", key)
		}
	}
	// 摘要只能出现在专用字段里，不能混进令牌元数据视图。
	if strings.Contains(string(raw["apiTokens"]), "hash-secret") {
		t.Error("API 令牌摘要泄露到 apiTokens 快照")
	}
	if !strings.Contains(string(raw["apiTokenHashes"]), "hash-secret") {
		t.Error("API 令牌摘要未落盘")
	}

	// 重新加载，验证内容完整（尤其 tokens 回填）
	s2 := New(dir)
	got, ok := s2.Node("n1")
	if !ok {
		t.Fatal("重载后节点丢失")
	}
	if got.Token != "tok-secret" {
		t.Errorf("令牌未持久化: got %q, want %q", got.Token, "tok-secret")
	}
	if got.Name != "节点一" || got.Provider != "test" {
		t.Errorf("节点字段丢失: %+v", got)
	}
	if h := s2.Host("n1"); h.Hostname != "h1" || h.OS != "Debian 12" {
		t.Errorf("host 丢失: %+v", h)
	}
	if m, ts, ok := s2.Latest("n1"); !ok || m.CPUPct != 42.5 || ts != 1000 {
		t.Errorf("指标丢失: %+v ts=%d ok=%v", m, ts, ok)
	}
	if len(s2.Services()) != 1 {
		t.Errorf("服务丢失: %d", len(s2.Services()))
	}
	if len(s2.Rules()) != 1 {
		t.Errorf("规则丢失: %d", len(s2.Rules()))
	}
	if len(s2.Alerts(0)) != 1 {
		t.Errorf("告警丢失: %d", len(s2.Alerts(0)))
	}
	if s2.GetSetting("notify") != `{"enabled":true}` {
		t.Errorf("settings 丢失: %q", s2.GetSetting("notify"))
	}
	if _, ok := s2.MatchAPIToken("hash-secret", 2000); !ok {
		t.Error("重启后 API 令牌无法匹配")
	}
	toks := s2.APITokens()
	if len(toks) != 1 || len(toks[0].Scopes) != 1 || toks[0].Scopes[0] != "read" {
		t.Errorf("API 令牌元数据丢失: %+v", toks)
	}
	if len(toks) == 1 && toks[0].Hash != "" {
		t.Error("APITokens 视图泄露摘要")
	}

	// 令牌不得出现在公开视图（Nodes() 走 json:"-"）
	pub, _ := json.Marshal(s2.Nodes())
	if strings.Contains(string(pub), "tok-secret") {
		t.Error("令牌泄露到公开节点视图")
	}
}

// TestSaveRecordsPhaseTimings 是分段计时的接线回归。
//
// 加这套指标的目的，是在优化快照前先定位瓶颈；如果哪次重构把某个阶段
// 的计时弄丢（例如改成流式编码后 Marshal 恒为 0），指标会静默失真，
// 后续所有"优化"就都建立在假数据上。
func TestSaveRecordsPhaseTimings(t *testing.T) {
	s := New(t.TempDir())
	// 塞够数据：快照太小时 gzip 会快到计时为 0，指标"接线正确"也无法验证。
	// 2000 个历史点约 1MB JSON，压缩耗时足够可观。
	for i := 0; i < 20; i++ {
		s.UpsertNode(&model.Node{ID: "n" + strconv.Itoa(i), Name: "节点" + strconv.Itoa(i)})
	}
	for i := 0; i < 2000; i++ {
		s.AddMetrics("n0", model.HostInfo{Hostname: "n0"},
			model.Metrics{TS: int64(1700000000 + i*30), CPUPct: 12.5, MemUsed: 1 << 30, MemTotal: 8 << 30,
				NetRx: uint64(i) * 1024, NetTx: uint64(i) * 512})
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	st := s.SaveStats()
	if st.LastBytes <= 0 {
		t.Fatalf("快照体积未记录: %+v", st)
	}
	sum := st.LastCopySeconds + st.LastMarshalSeconds + st.LastEncodeSeconds + st.LastWriteSeconds + st.LastSeriesSeconds
	if sum <= 0 {
		t.Fatalf("分段耗时全为 0，指标未接线: %+v", st)
	}
	// 主快照现在只含配置与当前态（几百字节到 1MB），编码耗时可能被舍入成 0；
	// 真正的大头移到了历史分片，所以这里断言"分片阶段确实发生了"，
	// 而不是硬要求 marshal 非 0。
	if st.LastSeriesFiles == 0 || st.LastSeriesBytes == 0 {
		t.Fatalf("历史分片阶段未记录（应写出至少一个分片）: %+v", st)
	}
	if st.LastSeriesSeconds <= 0 {
		t.Fatalf("分片耗时未记录: %+v", st)
	}
	// 分段之和不应超过总耗时（总耗时还包含锁等待与 defer 开销）
	if sum > st.LastDurationSeconds+0.05 {
		t.Fatalf("分段之和 %.4fs 超过总耗时 %.4fs", sum, st.LastDurationSeconds)
	}
}

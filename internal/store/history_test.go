package store

import (
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/model"
	"runtime"
)

// histBase 是 2023-11-15 00:00:00 UTC，可被 60/300/3600 整除。
const histBase = int64(1700006400)

func addHistSample(s *Store, id string, i int) {
	s.AddMetrics(id, model.HostInfo{}, model.Metrics{
		TS:       histBase + int64(i)*30,
		CPUPct:   float64(i + 1),
		Load1:    float64(i) / 10,
		MemUsed:  2 << 30,
		MemTotal: 8 << 30,
		DiskUsed: 20 << 30, DiskTotal: 60 << 30,
		NetRx: 1000 + uint64(100*i),
		NetTx: 2000 + uint64(50*i),
	})
}

func approx(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.01 {
		t.Fatalf("got %.4f, want %.4f", got, want)
	}
}

func TestHistoryAggregation(t *testing.T) {
	s := New(t.TempDir())
	for i := 0; i < 20; i++ { // 10 分钟，每 30 秒一个点
		addHistSample(s, "n1", i)
	}

	counts := s.HistoryCounts("n1")
	if counts[HistoryResolution1m] != 10 || counts[HistoryResolution5m] != 2 || counts[HistoryResolution1h] != 1 {
		t.Fatalf("桶数量不符: %+v", counts)
	}

	m1 := s.HistorySeries("n1", HistoryResolution1m, histBase)
	if len(m1) != 10 {
		t.Fatalf("1m 点数 %d，want 10", len(m1))
	}
	if m1[0].TS != histBase || m1[0].Count != 2 {
		t.Fatalf("首个 1m 桶异常: %+v", m1[0])
	}
	approx(t, m1[0].CPUPct, 1.5)
	approx(t, m1[0].CPUMax, 2)
	approx(t, m1[0].MemPct, 25)
	approx(t, m1[0].DiskPct, 100.0/3)
	approx(t, m1[0].Load1, 0.05)
	if m1[0].NetRxDelta != 100 || m1[0].NetTxDelta != 50 {
		t.Fatalf("首个桶流量增量异常: rx=%d tx=%d", m1[0].NetRxDelta, m1[0].NetTxDelta)
	}
	approx(t, float64(m1[1].CPUPct), 3.5)

	m5 := s.HistorySeries("n1", HistoryResolution5m, histBase)
	if len(m5) != 2 || m5[0].Count != 10 {
		t.Fatalf("5m 聚合异常: %+v", m5)
	}
	approx(t, m5[0].CPUPct, 5.5)
	if m5[0].NetRxDelta != 900 {
		t.Fatalf("5m 接收增量 %d，want 900", m5[0].NetRxDelta)
	}

	h1 := s.HistorySeries("n1", HistoryResolution1h, histBase)
	if len(h1) != 1 || h1[0].Count != 20 {
		t.Fatalf("1h 聚合异常: %+v", h1)
	}
	approx(t, h1[0].CPUPct, 10.5)
	approx(t, h1[0].CPUMax, 20)
	if h1[0].NetRxDelta != 1900 || h1[0].NetTxDelta != 950 {
		t.Fatalf("1h 流量增量异常: rx=%d tx=%d", h1[0].NetRxDelta, h1[0].NetTxDelta)
	}
	if h1[0].MemUsed == 0 || h1[0].DiskUsed == 0 {
		t.Fatalf("聚合桶未回算出已用字节: %+v", h1[0])
	}

	// since 过滤：只返回桶起始时间不早于 since 的桶。
	got := s.HistorySeries("n1", HistoryResolution1m, histBase+300)
	if len(got) != 5 || got[0].TS != histBase+300 {
		t.Fatalf("since 过滤异常: len=%d first=%+v", len(got), got)
	}
}

func TestConfigureHistoryValidation(t *testing.T) {
	t.Cleanup(func() { _ = ConfigureHistory(DefaultHistoryRetention()) })
	if err := ConfigureHistory(HistoryRetention{Tier1m: 2 * time.Hour, Tier5m: 12 * time.Hour, Tier1h: 48 * time.Hour}); err != nil {
		t.Fatalf("配置有效保留期失败: %v", err)
	}
	if tier1m.keep != 120 || tier5m.keep != 144 || tier1h.keep != 48 {
		t.Fatalf("桶数量未按保留期更新: %+v %+v %+v", tier1m, tier5m, tier1h)
	}
	bad := []HistoryRetention{
		{Tier1m: 30 * time.Second, Tier5m: time.Hour, Tier1h: time.Hour},
		{Tier1m: time.Hour, Tier5m: 5 * time.Minute, Tier1h: time.Hour},
		{Tier1m: 90 * time.Minute, Tier5m: time.Hour, Tier1h: time.Hour},
		{Tier1m: 11 * 365 * 24 * time.Hour, Tier5m: time.Hour, Tier1h: time.Hour},
	}
	for _, r := range bad {
		if err := ConfigureHistory(r); err == nil {
			t.Fatalf("非法保留期应报错: %+v", r)
		}
	}
}

func TestHistoryUsageSumsPreserveAbsoluteValues(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	s.AddMetrics("n1", model.HostInfo{}, model.Metrics{
		TS: histBase, MemUsed: 1 << 30, MemTotal: 2 << 30,
		DiskUsed: 10 << 30, DiskTotal: 60 << 30,
	})
	s.AddMetrics("n1", model.HostInfo{}, model.Metrics{
		TS: histBase + 30, MemUsed: 3 << 30, MemTotal: 8 << 30,
		DiskUsed: 20 << 30, DiskTotal: 60 << 30,
	})
	check := func(s *Store, label string) {
		t.Helper()
		m1 := s.HistorySeries("n1", HistoryResolution1m, histBase)
		if len(m1) != 1 {
			t.Fatalf("%s: 1m 点数 %d", label, len(m1))
		}
		if m1[0].MemUsed != 2<<30 {
			t.Fatalf("%s: 平均内存用量 %d，want %d（不能用最新总量反推）", label, m1[0].MemUsed, uint64(2<<30))
		}
		if m1[0].DiskUsed != 15<<30 {
			t.Fatalf("%s: 平均磁盘用量 %d，want %d", label, m1[0].DiskUsed, uint64(15<<30))
		}
	}
	check(s, "内存")
	if err := s.SaveHistory(); err != nil {
		t.Fatal(err)
	}
	s2 := New(dir)
	if err := s2.LoadErr(); err != nil {
		t.Fatal(err)
	}
	check(s2, "重载")
}

// TestHistorySchema1Fallback 验证旧快照没有绝对用量总和时按最新总量回算，
// 并在同桶新增样本时把旧样本补进总和，避免平均值被拉低。
func TestHistorySchema1Fallback(t *testing.T) {
	dir := t.TempDir()
	old := `{"schemaVersion":1,"nodes":{"n1":{"lastTs":1700006430,"m1":[` +
		`{"b":1700006400,"c":2,"ca":1,"cm":1,"ma":25,"da":33.333333,"mt":8589934592,"dt":64424509440}]}}}`
	if err := os.WriteFile(filepath.Join(dir, "history.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(dir)
	if err := s.LoadErr(); err != nil {
		t.Fatal(err)
	}
	m1 := s.HistorySeries("n1", HistoryResolution1m, histBase)
	if len(m1) != 1 || m1[0].MemUsed != 2<<30 {
		t.Fatalf("旧 schema 回算异常: %+v", m1)
	}
	s.AddMetrics("n1", model.HostInfo{}, model.Metrics{
		TS: histBase + 30, MemUsed: 4 << 30, MemTotal: 8 << 30,
		DiskUsed: 40 << 30, DiskTotal: 60 << 30,
	})
	m1 = s.HistorySeries("n1", HistoryResolution1m, histBase)
	if len(m1) != 1 || m1[0].Count != 3 {
		t.Fatalf("旧桶续写异常: %+v", m1)
	}
	// (2GB×2 旧样本 + 4GB) / 3
	if want := uint64(8<<30) / 3; m1[0].MemUsed != want {
		t.Fatalf("旧桶续写后平均用量 %d，want %d", m1[0].MemUsed, want)
	}
}

func TestBucketRingRetention(t *testing.T) {
	r := &bucketRing{interval: 60, size: 3}
	for _, ts := range []int64{600, 660, 720, 780} {
		r.add(model.Metrics{TS: ts, CPUPct: float64(ts / 60)}, nil, false, false)
	}
	got := r.slice()
	if len(got) != 3 {
		t.Fatalf("保留桶数 %d，want 3", len(got))
	}
	for i, want := range []int64{660, 720, 780} {
		if got[i].Index != want {
			t.Fatalf("第 %d 个桶 index=%d，want %d", i, got[i].Index, want)
		}
	}
	// 同桶追加不应新增桶。
	r.add(model.Metrics{TS: 799, CPUPct: 99}, nil, false, false)
	if n := len(r.slice()); n != 3 {
		t.Fatalf("同桶追加后桶数 %d，want 3", n)
	}
}

func TestHistoryPersistsAndReplays(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows 没有 POSIX 权限位：chmod 设置的可执行位/0600 读回来仍是 666，
		// 这里断言的是 Linux 上的安全语义，跳过而不是让整个仓库在 Windows 上红灯。
		t.Skip("Windows 无 POSIX 权限位，跳过文件权限断言")
	}
	dir := t.TempDir()
	s := New(dir)
	// 历史分片以节点为准持久化（没有节点记录的分片会被当作孤儿清理），
	// 所以这里必须先登记节点 —— 真实上报路径也一定先解析出节点。
	s.UpsertNode(&model.Node{ID: "n1", Name: "n1"})
	for i := 0; i < 20; i++ {
		addHistSample(s, "n1", i)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s.SaveHistory(); err != nil {
		t.Fatalf("SaveHistory: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, "history.json"))
	if err != nil {
		t.Fatalf("history.json 未生成: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("history.json 权限 %o，want 600", fi.Mode().Perm())
	}

	s2 := New(dir)
	if err := s2.LoadErr(); err != nil {
		t.Fatalf("重载: %v", err)
	}
	counts := s2.HistoryCounts("n1")
	if counts[HistoryResolution1m] != 10 || counts[HistoryResolution5m] != 2 || counts[HistoryResolution1h] != 1 {
		t.Fatalf("重载后桶数量不符: %+v", counts)
	}
	// 恢复到已聚合到的时间点后继续上报，不应重复累计。
	for i := 20; i < 30; i++ {
		addHistSample(s2, "n1", i)
	}
	h1 := s2.HistorySeries("n1", HistoryResolution1h, histBase)
	if len(h1) != 1 || h1[0].Count != 30 {
		t.Fatalf("增量恢复后聚合异常: %+v", h1)
	}
	if h1[0].NetRxDelta != 2900 || h1[0].NetTxDelta != 1450 {
		t.Fatalf("重启后流量重复累计: rx=%d tx=%d", h1[0].NetRxDelta, h1[0].NetTxDelta)
	}
}

func TestHistoryRebuildsFromRaw(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	// 历史分片按节点归属加载：没有节点记录的分片会被当作孤儿跳过，
	// 所以这里必须先登记节点（真实上报路径也一定先解析出节点）。
	s.UpsertNode(&model.Node{ID: "n1", Name: "n1"})
	for i := 0; i < 20; i++ {
		addHistSample(s, "n1", i)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// 不写 history.json，模拟旧版本升级或历史文件丢失。
	s2 := New(dir)
	if err := s2.LoadErr(); err != nil {
		t.Fatalf("重载: %v", err)
	}
	counts := s2.HistoryCounts("n1")
	if counts[HistoryResolution1m] != 10 || counts[HistoryResolution5m] != 2 || counts[HistoryResolution1h] != 1 {
		t.Fatalf("回填桶数量不符: %+v", counts)
	}
	h1 := s2.HistorySeries("n1", HistoryResolution1h, histBase)
	// 回填时首个原始点没有前值，流量只累计 19 次增量。
	if len(h1) != 1 || h1[0].NetRxDelta != 1900 {
		t.Fatalf("回填结果异常: %+v", h1)
	}
}

func TestHistoryCorruptFileQuarantined(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	s.UpsertNode(&model.Node{ID: "n1", Name: "n1"})
	for i := 0; i < 20; i++ {
		addHistSample(s, "n1", i)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "history.json"), []byte("not-json"), 0o600); err != nil {
		t.Fatalf("写入损坏文件: %v", err)
	}

	s2 := New(dir)
	if err := s2.LoadErr(); err != nil {
		t.Fatalf("损坏历史应回填而不是失败: %v", err)
	}
	if counts := s2.HistoryCounts("n1"); counts[HistoryResolution1m] != 10 {
		t.Fatalf("损坏后回填异常: %+v", counts)
	}
	baks, _ := filepath.Glob(filepath.Join(dir, "history.json.corrupt-*"))
	if len(baks) != 1 {
		t.Fatalf("损坏文件未隔离: %v", baks)
	}
}

func TestHistoryNewerSchemaRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.json")
	if err := os.WriteFile(path, []byte(`{"schemaVersion":99,"nodes":{}}`), 0o600); err != nil {
		t.Fatalf("写入新版本文件: %v", err)
	}
	s := New(dir)
	if s.LoadErr() == nil {
		t.Fatal("更高 schema 版本应拒绝启动")
	}
	// 文件必须原样保留：改名只能挡住一次启动，第二次启动会「找不到文件」
	// 并用 24h 原始点重建覆盖，把新版本写入的历史永久丢掉。
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("新版本文件不应被移走: %v", err)
	}
	if baks, _ := filepath.Glob(filepath.Join(dir, "history.json.newer-*")); len(baks) != 0 {
		t.Fatalf("不应改名备份: %v", baks)
	}
	if err := os.WriteFile(path, []byte(`{"schemaVersion":99,"nodes":{}}`), 0o600); err != nil {
		t.Fatalf("重写新版本文件: %v", err)
	}
	if s2 := New(dir); s2.LoadErr() == nil {
		t.Fatal("第二次启动也必须继续拒绝（防止降级覆盖）")
	}
}

func TestDeleteNodeRemovesHistory(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	s.UpsertNode(&model.Node{ID: "n1", Name: "n1"})
	for i := 0; i < 5; i++ {
		addHistSample(s, "n1", i)
	}
	s.DeleteNode("n1")
	if counts := s.HistoryCounts("n1"); counts[HistoryResolution1m] != 0 {
		t.Fatalf("删除节点后历史仍存在: %+v", counts)
	}
	if err := s.SaveHistory(); err != nil {
		t.Fatalf("SaveHistory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "history.json")); err != nil {
		t.Fatalf("history.json 未生成: %v", err)
	}
}

// TestHistoryConcurrentAccess 复现「写入聚合 + 定期落盘 + 查询」并发路径。
// 没有 -race 时，map 并发读写仍会触发 runtime 的 fatal error。
func TestHistoryConcurrentAccess(t *testing.T) {
	s := New(t.TempDir())
	s.UpsertNode(&model.Node{ID: "n1", Name: "n1"})

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			select {
			case <-stop:
				return
			default:
			}
			addHistSample(s, "n1", i)
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				if err := s.SaveHistory(); err != nil {
					t.Errorf("SaveHistory: %v", err)
					return
				}
			}
		}
	}()

	for i := 0; i < 100; i++ {
		_ = s.HistorySeries("n1", HistoryResolution1m, histBase)
		_ = s.HistoryCounts("n1")
	}
	close(stop)
	wg.Wait()
}

func BenchmarkSaveHistory40Nodes(b *testing.B) {
	s := newBenchStore(b, 40)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.mu.Lock()
		s.historyDirty = true
		s.mu.Unlock()
		if err := s.SaveHistory(); err != nil {
			b.Fatal(err)
		}
	}
}

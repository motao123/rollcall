package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/model"
)

func seedNode(t *testing.T, s *Store, id string, points int) {
	t.Helper()
	if _, ok := s.Node(id); !ok {
		s.UpsertNode(&model.Node{ID: id, Name: id})
	}
	ts := time.Now().Unix() - int64(points*30)
	for i := 0; i < points; i++ {
		s.AddMetrics(id, model.HostInfo{Hostname: id}, model.Metrics{
			TS: ts + int64(i*30), CPUPct: float64(i % 100),
			MemUsed: 1 << 20, MemTotal: 2 << 20, DiskUsed: 1 << 20, DiskTotal: 4 << 20,
			NetRx: uint64(i) * 100, NetTx: uint64(i) * 50,
		})
	}
}

// TestSeriesShardRoundTrip 覆盖分片的核心契约：
// Save 之后主快照不再内嵌 series，新实例从 data/series 读回同样多的点。
func TestSeriesShardRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	seedNode(t, s, "n1", 50)
	seedNode(t, s, "n2", 20)
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	for _, id := range []string{"n1", "n2"} {
		if _, err := os.Stat(filepath.Join(dir, "series", id+".json.gz")); err != nil {
			t.Fatalf("%s 的分片未写出: %v", id, err)
		}
	}
	// 主快照应当很小：series 移走后只剩配置与当前态。
	info, err := os.Stat(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 64<<10 {
		t.Fatalf("主快照仍过大（%d 字节），series 可能没被拆出去", info.Size())
	}

	re := New(dir)
	if err := re.LoadErr(); err != nil {
		t.Fatalf("重载: %v", err)
	}
	for id, want := range map[string]int{"n1": 50, "n2": 20} {
		got := re.Series(id, 0)
		if len(got) != want {
			t.Fatalf("%s 分片点数不符: got %d want %d", id, len(got), want)
		}
		// 顺序必须升序：Ring.Add 依赖这个前提
		for i := 1; i < len(got); i++ {
			if got[i].TS <= got[i-1].TS {
				t.Fatalf("%s 分片内时间未升序: %d then %d", id, got[i-1].TS, got[i].TS)
			}
		}
	}
}

// TestSeriesShardIncremental 覆盖分片节流与强制刷盘：
//
// 分片不再跟随每次 Save（30 秒上报 + 60 秒保存下每次几乎全脏，增量没意义），
// 而是默认 5 分钟一次；因此这里验证三件事：
//   - 首次 Save 写出全部分片
//   - 间隔内的第二次 Save 不写分片（主快照照常写）
//   - FlushSeries 忽略间隔，把变脏的节点补写
func TestSeriesShardIncremental(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	seedNode(t, s, "n1", 10)
	seedNode(t, s, "n2", 10)
	if err := s.Save(); err != nil {
		t.Fatalf("首次 Save: %v", err)
	}
	if got := s.SaveStats().LastSeriesFiles; got != 2 {
		t.Fatalf("首次 Save 应写 2 个分片，实际 %d", got)
	}

	// 间隔内的第二次 Save：不应写分片（节流生效）
	if err := s.Save(); err != nil {
		t.Fatalf("第二次 Save: %v", err)
	}
	if got := s.SaveStats().LastSeriesFiles; got != 0 {
		t.Fatalf("间隔内不应写分片，实际写了 %d 个", got)
	}

	// 只有 n1 有新数据；Save 仍受节流限制
	s.AddMetrics("n1", model.HostInfo{Hostname: "n1"}, model.Metrics{
		TS: time.Now().Unix() + 5, CPUPct: 42,
	})
	if err := s.Save(); err != nil {
		t.Fatalf("第三次 Save: %v", err)
	}
	if got := s.SaveStats().LastSeriesFiles; got != 0 {
		t.Fatalf("节流期间不应写分片，实际 %d", got)
	}

	// 强制刷盘：只写变脏的 1 个
	if err := s.FlushSeries(); err != nil {
		t.Fatalf("FlushSeries: %v", err)
	}
	stats := s.SaveStats()
	_ = stats
	// FlushSeries 不污染 SaveStats，这里直接确认分片内容已更新
	re := New(dir)
	if err := re.LoadErr(); err != nil {
		t.Fatalf("重载: %v", err)
	}
	if got := re.Series("n1", 0); len(got) != 11 {
		t.Fatalf("强制刷盘后 n1 应有 11 点，实际 %d", len(got))
	}
}

// TestFlushSeriesWritesOnlyDirty 确认强制刷盘不会重写未变脏的节点。
func TestFlushSeriesWritesOnlyDirty(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	seedNode(t, s, "n1", 5)
	seedNode(t, s, "n2", 5)
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// 所有节点都已保存，强制刷盘应无事可做
	if err := s.FlushSeries(); err != nil {
		t.Fatalf("FlushSeries: %v", err)
	}
	s.AddMetrics("n2", model.HostInfo{Hostname: "n2"}, model.Metrics{TS: time.Now().Unix() + 5})
	before := fileModTime(t, filepath.Join(dir, "series", "n1.json.gz"))
	if err := s.FlushSeries(); err != nil {
		t.Fatalf("FlushSeries: %v", err)
	}
	if after := fileModTime(t, filepath.Join(dir, "series", "n1.json.gz")); after != before {
		t.Fatal("未变脏的节点分片不应被重写")
	}
}

func fileModTime(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.ModTime().UnixNano()
}

// TestSeriesShardCorruptionIsolated 覆盖损坏隔离：
// 一个分片损坏不应让整个加载失败，也不应影响其它节点的历史。
func TestSeriesShardCorruptionIsolated(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	seedNode(t, s, "good", 10)
	seedNode(t, s, "bad", 10)
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "series", "bad.json.gz"), []byte("not gzip"), 0o600); err != nil {
		t.Fatal(err)
	}

	re := New(dir)
	if err := re.LoadErr(); err != nil {
		t.Fatalf("单个分片损坏不应导致启动失败: %v", err)
	}
	if got := re.Series("good", 0); len(got) != 10 {
		t.Fatalf("正常节点的历史丢失: %d 点", len(got))
	}
	if got := re.Series("bad", 0); len(got) != 0 {
		t.Fatalf("损坏分片应被跳过，实际读到 %d 点", len(got))
	}
}

// TestDeleteNodeRemovesShard 覆盖删除语义：
// 删节点要同时删掉它的分片，否则磁盘会永久留下孤儿文件。
func TestDeleteNodeRemovesShard(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	seedNode(t, s, "n1", 10)
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	path := filepath.Join(dir, "series", "n1.json.gz")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("分片未写出: %v", err)
	}
	if !s.DeleteNode("n1") {
		t.Fatal("删除节点失败")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("节点删除后分片应被移除，实际 stat: %v", err)
	}
}

// TestPruneRemovesOrphanShards 覆盖兜底清理：
// 分片存在但节点已不在（例如手工恢复、导入备份）时，Save 应清掉它。
func TestPruneRemovesOrphanShards(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	seedNode(t, s, "n1", 5)
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	orphan := filepath.Join(dir, "series", "ghost.json.gz")
	if err := os.WriteFile(orphan, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("第二次 Save: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("孤儿分片应被清理，实际 stat: %v", err)
	}
}

// TestShardFileNameSanitizes 覆盖文件名安全：
// 导入备份可能带来含路径分隔符的节点 ID，不能用来拼路径。
func TestShardFileNameSanitizes(t *testing.T) {
	cases := []string{"n_abc", "", "../../etc/passwd", "a/b", "id with space"}
	for _, id := range cases {
		name := shardFileName(id)
		if filepath.Base(name) != name {
			t.Fatalf("ID %q 生成的文件名含路径分隔符: %q", id, name)
		}
		if len(name) > 200 {
			t.Fatalf("ID %q 生成的文件名过长: %d", id, len(name))
		}
	}
}

// TestHistoryReplayFromShardsNoDoubleCount 是回归：
//
// 重启后应当从分片读回原始点，并接着 history.json 的聚合继续累计，
// 既不重复计流量也不漏一次增量。曾因"分片加载要求节点存在"而丢点：
// 测试/恢复场景里节点记录可能缺失，导致重启后第一次上报被当成无前值。
func TestHistoryReplayFromShardsNoDoubleCount(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
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

	re := New(dir)
	if err := re.LoadErr(); err != nil {
		t.Fatalf("重载: %v", err)
	}
	if got := len(re.Series("n1", 0)); got != 20 {
		t.Fatalf("重启后应从分片读回 20 个点，实际 %d", got)
	}
	for i := 20; i < 30; i++ {
		addHistSample(re, "n1", i)
	}
	h := re.HistorySeries("n1", HistoryResolution1h, histBase)
	if len(h) != 1 || h[0].Count != 30 {
		t.Fatalf("聚合异常: %+v", h)
	}
	if h[0].NetRxDelta != 2900 || h[0].NetTxDelta != 1450 {
		t.Fatalf("重启后流量累计错误: rx=%d tx=%d", h[0].NetRxDelta, h[0].NetTxDelta)
	}
}

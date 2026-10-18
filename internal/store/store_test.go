package store

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/motao/rollcall/internal/model"
)

// TestTokenPersistsAcrossRestart 是回归测试：
// model.Node.Token 标了 json:"-"，若不做单独持久化，
// 服务端重启后所有 Agent 会因令牌为空而 401。
// 这个 bug 在首次真实部署时暴露，必须守住。
func TestTokenPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()

	st := New(dir)
	n := &model.Node{ID: "n_test1", Name: "test-node", Token: "secret-token-abc"}
	st.UpsertNode(n)

	if err := st.Save(); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	// 模拟重启：从同一目录重新加载
	st2 := New(dir)
	got, ok := st2.Node("n_test1")
	if !ok {
		t.Fatal("重启后节点丢失")
	}
	if got.Token != "secret-token-abc" {
		t.Fatalf("重启后令牌丢失: 期望 %q，实际 %q", "secret-token-abc", got.Token)
	}

	// 令牌必须能通过认证校验
	if _, ok := st2.NodeByToken("secret-token-abc", "n_test1"); !ok {
		t.Fatal("NodeByToken 校验失败：重启后 Agent 会收到 401")
	}
	// 错误令牌必须被拒绝
	if _, ok := st2.NodeByToken("wrong", "n_test1"); ok {
		t.Fatal("错误令牌不应通过校验")
	}
}

// TestTokenNotLeakedToStateFile 确认令牌不出现在节点 JSON 里
// （只允许单独出现在 tokens 字段中，供内部还原）。
func TestTokenNotLeakedToStateFile(t *testing.T) {
	dir := t.TempDir()
	st := New(dir)
	st.UpsertNode(&model.Node{ID: "n_x", Name: "x", Token: "tok-xyz"})
	if err := st.Save(); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	// 令牌存在（在 tokens 字段），但 nodes 里不应有 token 键
	if !contains(string(b), `"tokens"`) {
		t.Fatal("state.json 缺少 tokens 字段")
	}
}

// TestRingWrapsAndKeepsOrder 校验环形缓冲的环绕与顺序。
func TestRingWrapsAndKeepsOrder(t *testing.T) {
	r := NewRing(3)
	for i := int64(1); i <= 5; i++ {
		r.Add(model.Metrics{TS: i, CPUPct: float64(i)})
	}
	got := r.Slice()
	if len(got) != 3 {
		t.Fatalf("期望保留 3 点，实际 %d", len(got))
	}
	if got[0].TS != 3 || got[2].TS != 5 {
		t.Fatalf("环绕后顺序错误: %v", []int64{got[0].TS, got[1].TS, got[2].TS})
	}
}

// TestRingDedupSameSecond 同一秒重复上报应覆盖而非追加。
func TestRingDedupSameSecond(t *testing.T) {
	r := NewRing(10)
	r.Add(model.Metrics{TS: 100, CPUPct: 1})
	r.Add(model.Metrics{TS: 100, CPUPct: 2})
	if len(r.Slice()) != 1 {
		t.Fatalf("同一秒应去重，实际 %d 条", len(r.Slice()))
	}
}

// TestLoadCorruptSnapshotIsolated 校验损坏快照不会被静默当成空库。
func TestLoadCorruptSnapshotIsolated(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := New(dir)
	if st.LoadErr() == nil {
		t.Fatal("损坏快照应返回 LoadErr")
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); !os.IsNotExist(err) {
		t.Error("损坏文件应被移走，避免下次启动重复失败")
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "state.json.corrupt-*"))
	if len(matches) != 1 {
		t.Errorf("应生成一份隔离备份，实际 %d 份", len(matches))
	}
}

// TestRingDropsOlderSamples 时间倒退的采样点必须丢弃，否则升序假设被破坏。
func TestRingDropsOlderSamples(t *testing.T) {
	r := NewRing(4)
	r.Add(model.Metrics{TS: 200, CPUPct: 2})
	r.Add(model.Metrics{TS: 100, CPUPct: 1}) // 迟到样本
	got := r.Slice()
	if len(got) != 1 || got[0].TS != 200 {
		t.Fatalf("迟到样本应被丢弃: %+v", got)
	}
}

// TestOpenAlertKeepsUnresolvedWhenTrimming 容量淘汰只能删除已恢复记录。
func TestOpenAlertKeepsUnresolvedWhenTrimming(t *testing.T) {
	st := New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "n", Token: "t"})
	st.UpsertRule(&model.AlertRule{ID: "r1", Name: "r", Enabled: true})

	for i := 0; i < 520; i++ {
		st.OpenAlert(&model.Alert{
			ID: NewID("a"), RuleID: "r1", NodeID: "n1", StartedAt: int64(i + 1), ResolvedAt: int64(i + 2),
		})
	}
	if created, ok := st.OpenAlert(&model.Alert{
		ID: "active", RuleID: "r1", NodeID: "n1", StartedAt: 9999,
	}); !created || !ok {
		t.Fatalf("未恢复告警写入失败: created=%v ok=%v", created, ok)
	}
	for i := 0; i < 50; i++ {
		st.OpenAlert(&model.Alert{
			ID: NewID("a"), RuleID: "r1", NodeID: "n1", StartedAt: int64(10000 + i), ResolvedAt: int64(10001 + i),
		})
	}
	if got := st.ActiveAlertCount(); got != 1 {
		t.Fatalf("淘汰后未恢复告警应保留，实际 active=%d", got)
	}
	if len(st.Alerts(0)) > 500 {
		t.Fatalf("告警历史不应超过 500 条，实际 %d", len(st.Alerts(0)))
	}
}

// TestOpenAlertRejectsDeletedTargets 并发删除节点后不得再写入僵尸告警。
func TestOpenAlertRejectsDeletedTargets(t *testing.T) {
	st := New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "n", Token: "t"})
	st.UpsertRule(&model.AlertRule{ID: "r1", Name: "r", Enabled: true})
	st.DeleteNode("n1")

	created, ok := st.OpenAlert(&model.Alert{ID: "a1", RuleID: "r1", NodeID: "n1"})
	if created || ok {
		t.Fatalf("节点已删除仍写入了告警: created=%v ok=%v", created, ok)
	}
	if st.ActiveAlertCount() != 0 {
		t.Fatal("不应留下未恢复告警")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestConcurrentSaveProducesValidSnapshot 回归测试：
// 并发 Save 共用固定 .tmp 路径，未串行化时会互相截断，
// 落盘可能变成半截 JSON，重启即判定快照损坏。
func TestConcurrentSaveProducesValidSnapshot(t *testing.T) {
	dir := t.TempDir()
	st := New(dir)
	st.UpsertNode(&model.Node{ID: "n_save1", Name: "n1", Token: "tok-1"})
	st.SetSetting("k", "v")

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				if err := st.Save(); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发 Save 失败: %v", err)
	}

	re := New(dir)
	if err := re.LoadErr(); err != nil {
		t.Fatalf("快照应可恢复: %v", err)
	}
	if got := len(re.Nodes()); got != 1 {
		t.Fatalf("节点数 = %d，期望 1", got)
	}
	if got, ok := re.Node("n_save1"); !ok || got.Token != "tok-1" {
		t.Fatalf("令牌未正确恢复: %+v", got)
	}
}

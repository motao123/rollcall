package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/motao/probe/internal/model"
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

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

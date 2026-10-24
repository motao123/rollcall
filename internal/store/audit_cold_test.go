package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/model"
)

// TestAuditColdQuery 验证内存窗口装满后，更早的记录仍能通过文件回源查到。
// 构造：写入 auditMemoryLimit+10 条，最老 10 条 action=op-old（只在文件里），
// 其余 op-new。窗口未满时一切照旧（内存路径）。
func TestAuditColdQuery(t *testing.T) {
	s := New(t.TempDir())

	total := auditMemoryLimit + 10
	base := time.Now().Add(-48 * time.Hour).Unix()
	for i := 0; i < total; i++ {
		action := "op-new"
		if i < 10 {
			action = "op-old"
		}
		// TS 递增：内存窗口按追加顺序保留最后 1 万条，乱序会让"最老 10 条"
		// 不一定落在窗口外。
		s.AddAudit(auditEntry(action, base+int64(i)))
	}

	// 窗口必须已满，否则测试前提不成立。
	if n := s.AuditStats()["count"].(int); n != auditMemoryLimit {
		t.Fatalf("内存窗口应恰好装满 %d 条，实际 %d", auditMemoryLimit, n)
	}

	// 无时间条件 → 走冷路径，op-old 只有文件里有
	entries, cnt := s.QueryAudit(AuditQuery{Action: "op-old", Limit: 50})
	if cnt != 10 || len(entries) != 10 {
		t.Fatalf("冷查询应查到 10 条 op-old，实际 cnt=%d len=%d", cnt, len(entries))
	}
	if len(entries) > 0 && entries[0].Action != "op-old" {
		t.Fatalf("返回内容与过滤条件不符: %+v", entries[0])
	}

	// 冷路径的分页与总数
	_, cnt = s.QueryAudit(AuditQuery{Action: "op-new", Limit: 50})
	if cnt != auditMemoryLimit {
		t.Fatalf("op-new 总数应为 %d，实际 %d", auditMemoryLimit, cnt)
	}
	page2, cnt := s.QueryAudit(AuditQuery{Action: "op-old", Limit: 5, Offset: 5})
	if cnt != 10 || len(page2) != 5 {
		t.Fatalf("冷路径 offset 应生效：cnt=%d len=%d", cnt, len(page2))
	}
	if len(page2) > 0 && page2[0].TS >= base+10 {
		t.Fatalf("第二页应是最老的记录，TS=%d", page2[0].TS)
	}

	// 倒序：第一条应是该 action 最新的记录
	first, _ := s.QueryAudit(AuditQuery{Action: "op-old", Limit: 1})
	if len(first) == 1 && first[0].TS != base+9 {
		t.Fatalf("倒序错误：期望 TS=%d，实际 %d", base+9, first[0].TS)
	}

	// Since 晚于内存最早一条 → 不触发冷路径，也查不到 op-old
	mem, _ := s.QueryAudit(AuditQuery{Action: "op-old", Limit: 50, Since: base + 100})
	if len(mem) != 0 {
		t.Fatalf("时间范围内不应有 op-old，实际 %d 条", len(mem))
	}

	// 窗口未满时（新 store）全部可查，不需要冷路径
	s2 := New(t.TempDir())
	for i := 0; i < 5; i++ {
		s2.AddAudit(auditEntry("op", base+int64(i)))
	}
	entries2, cnt2 := s2.QueryAudit(AuditQuery{Action: "op"})
	if cnt2 != 5 || len(entries2) != 5 {
		t.Fatalf("窗口未满应走内存路径查到全部：cnt=%d len=%d", cnt2, len(entries2))
	}
}

// TestAuditColdQuerySearchMatch 验证冷路径与内存路径的搜索/时间过滤语义一致。
func TestAuditColdQuerySearchMatch(t *testing.T) {
	s := New(t.TempDir())

	base := time.Now().Add(-24 * time.Hour).Unix()
	for i := 0; i < auditMemoryLimit+3; i++ {
		e := auditEntry("nodes.update", base+int64(i))
		e.Detail = fmt.Sprintf("node-%d", i)
		if i == 0 {
			// 最老一条（只在文件里）给一个唯一哨兵串，避免子串匹配的歧义
			e.Detail = "sentinel-xyzzy"
		}
		s.AddAudit(e)
	}

	// search 命中最老一条（只在文件里）：哨兵串唯一，精确命中 1 条
	entries, cnt := s.QueryAudit(AuditQuery{Search: "sentinel-xyzzy", Limit: 10})
	if cnt != 1 || len(entries) != 1 {
		t.Fatalf("search 应精确命中 1 条哨兵，cnt=%d len=%d", cnt, len(entries))
	}
	if cnt == 1 && entries[0].Detail != "sentinel-xyzzy" {
		t.Fatalf("search 误配: %+v", entries[0])
	}
	// search 是子串匹配：node-1 同时命中 node-1 / node-10 / node-100…
	_, cntSub := s.QueryAudit(AuditQuery{Search: "node-1", Limit: 1})
	if cntSub < 2 {
		t.Fatalf("子串语义下 node-1 至少应命中 node-1 与 node-10，cnt=%d", cntSub)
	}
	_, cntAll := s.QueryAudit(AuditQuery{Search: "node-", Limit: 1})
	if cntAll != auditMemoryLimit+2 { // 除哨兵外其余都是 node-*
		t.Fatalf("子串匹配应命中全部 node-*：cnt=%d", cntAll)
	}
}

func auditEntry(action string, ts int64) (e model.AuditEntry) {
	e.Action = action
	e.TS = ts
	e.Actor = "tester"
	e.ID = NewID("au")
	return e
}

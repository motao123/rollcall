package web

import (
	"strconv"
	"testing"
	"time"
)

func fillerKey(i int) string {
	return "172.16." + strconv.Itoa(i/256%256) + "." + strconv.Itoa(i%256) + "|readonly"
}

// TestLimiterEvictionSkipsBlocked 是 P2 回归：
// 状态表打满时不能删除"正在封禁中"或"有在途名额"的条目。
//
// 旧实现在第一遍回收后仍超限时会无条件删除任意条目，正好把封禁清掉 ——
// 攻击者只要制造足够多的来源，就能让自己的封禁状态消失。
// 这里把整张表填成"全部在封禁中"，此时没有任何条目可以合法回收：
// 新实现一条都不删；旧实现会删掉一条（len 变成 max-1）。
func TestLimiterEvictionSkipsBlocked(t *testing.T) {
	l := newAuthFailureLimiter()
	now := time.Now()
	l.mu.Lock()
	for i := 0; i < maxLimiterEntries; i++ {
		l.state[fillerKey(i)] = &authFailState{
			windowStart: now, blockedUntil: now.Add(5 * time.Minute),
		}
	}
	l.state["10.0.0.1|readonly"] = &authFailState{windowStart: now, inflight: 1}
	before := len(l.state)
	l.evictLocked(now)
	after := len(l.state)
	l.mu.Unlock()

	if after != before {
		t.Fatalf("封禁中/在途条目被删除：%d → %d（限速器会在被攻击时主动解除封禁）", before, after)
	}
	if _, ok := l.state["10.0.0.1|readonly"]; !ok {
		t.Error("有在途名额的条目被删除，后续 fail/release 会重建并重置计数")
	}
}

// TestLimiterEvictionIsBounded 是 P2 回归：
// 单次淘汰的工作量必须有上限。旧实现在持有全局锁时遍历整张表，
// 高基数来源洪水会把限速器本身变成 CPU 放大器。
//
// 表里全是可以回收的过期条目：新实现最多删 evictBatch 条，
// 旧实现会一次删到接近空（len 骤降到个位数）。
func TestLimiterEvictionIsBounded(t *testing.T) {
	l := newAuthFailureLimiter()
	now := time.Now()
	l.mu.Lock()
	for i := 0; i < maxLimiterEntries; i++ {
		l.state[fillerKey(i)] = &authFailState{windowStart: now.Add(-2 * time.Hour)}
	}
	l.evictLocked(now)
	after := len(l.state)
	l.mu.Unlock()

	if after < maxLimiterEntries-evictBatch {
		t.Fatalf("单次淘汰删除了 %d 条（上限 %d），说明仍在做无界全表扫描",
			maxLimiterEntries-after, evictBatch)
	}
	if after >= maxLimiterEntries {
		t.Fatalf("过期条目一条都没回收：%d", after)
	}
}

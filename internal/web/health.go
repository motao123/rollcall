package web

import (
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/motao123/rollcall/internal/agentwire"
	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/store"
	"github.com/motao123/rollcall/internal/version"
)

// reportCounter 是上报次数的 60 秒滑动窗口。
//
// 按秒分桶而不是存时间戳切片：分桶大小固定（60 个 int64），
// 高上报速率下不会因为追加元素而持续分配内存。
type reportCounter struct {
	mu    sync.Mutex
	sec   [60]int64 // 每个桶对应的 unix 秒
	count [60]int64 // 该秒内的上报次数
}

// hit 记录一次上报。
func (c *reportCounter) hit(now int64) {
	slot := int(now % 60)
	c.mu.Lock()
	if c.sec[slot] != now {
		c.sec[slot] = now
		c.count[slot] = 0
	}
	c.count[slot]++
	c.mu.Unlock()
}

// last60s 返回最近 60 秒内的上报总数。
func (c *reportCounter) last60s(now int64) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var total int64
	for i := 0; i < 60; i++ {
		// 有效窗口是 [now-59, now]。允许最多 60 秒的负差值，
		// 让 NTP 小幅校正后旧桶仍计入；把大幅时钟回拨产生的
		// 远期桶排除掉，否则窗口会一直虚高到追上那个时间点。
		d := now - c.sec[i]
		if c.sec[i] != 0 && d > -60 && d < 60 {
			total += c.count[i]
		}
	}
	return total
}

// handleAdminHealth 返回实例自身的运行状态：内存、协程、上报速率、
// 快照各阶段耗时与通知队列。
//
// 这些数字此前只能从 /metrics 拉，出问题时最需要的恰恰是"面板上一眼看到"。
func (s *Server) handleAdminHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	now := time.Now()
	unix := now.Unix()

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	nodes := s.st.Nodes()
	online, stale, offline, noData := 0, 0, 0, 0
	for _, n := range nodes {
		_, lastSeen, ok := s.st.Latest(n.ID)
		if !ok {
			// 从未上报的节点是"待接入"，与曾经上报后失联的离线节点分开计数。
			noData++
			continue
		}
		switch store.NodeState(lastSeen, unix) {
		case model.NodeStateOnline:
			online++
		case model.NodeStateStale:
			stale++
		default:
			offline++
		}
	}

	ss := s.st.SaveStats()
	ns := s.st.NotifyStats()
	reports := s.reports.last60s(unix)
	lastSaveAgo := int64(0)
	if ss.LastSuccess > 0 {
		lastSaveAgo = unix - ss.LastSuccess
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"version":       version.Version,
		"uptimeSeconds": int64(now.Sub(s.startedAt).Seconds()),
		"goroutines":    runtime.NumGoroutine(),
		"memory": map[string]any{
			"heapAllocBytes": ms.HeapAlloc,
			"heapSysBytes":   ms.HeapSys,
			"numGC":          ms.NumGC,
		},
		"nodes": map[string]any{
			"total": len(nodes), "online": online, "stale": stale,
			"offline": offline, "noData": noData,
		},
		"reports": map[string]any{
			"last60s":   reports,
			"perSecond": float64(reports) / 60,
			// 预期只统计"可能还在上报"的节点：离线与从未上报的节点
			// 本来就不会发数据，把它们算进分母会让健康卡永久显示"采集掉队"。
			"expected":    expectedReportsPerMinute(online + stale),
			"intervalSec": agentwire.ReportInterval(agentwire.OnlineTTL),
		},
		"save": map[string]any{
			"count":                 ss.Count,
			"failures":              ss.Failures,
			"lastDurationMs":        ss.LastDurationSeconds * 1000,
			"copyMs":                ss.LastCopySeconds * 1000,
			"marshalMs":             ss.LastMarshalSeconds * 1000,
			"encodeMs":              ss.LastEncodeSeconds * 1000,
			"writeMs":               ss.LastWriteSeconds * 1000,
			"bytes":                 ss.LastBytes,
			"lastSuccessAgoSeconds": lastSaveAgo,
		},
		"notify": map[string]any{
			"pending": ns.Pending,
			"sent":    ns.Sent,
		},
		"limiterEntries": s.limiter.size(),
	})
}

// expectedReportsPerMinute 是按推荐上报间隔估算的每分钟应有上报数。
// 用它和实测值对比，能直接看出"采集是否掉队"。
func expectedReportsPerMinute(nodes int) int {
	interval := agentwire.ReportInterval(agentwire.OnlineTTL)
	if interval <= 0 {
		return 0
	}
	return nodes * (60 / interval)
}

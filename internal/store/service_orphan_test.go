package store

import (
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/model"
)

// TestServiceResultDroppedForDeletedService 覆盖"删服务时的迟到探测结果"。
//
// 生产上撞到的形态：删掉探测目标的那一刻恰好有一次探测在飞，结果落库时
// AddServiceResult 给已删服务重建了一个环 —— 这份数据既不会显示，也不会
// 自己过期，只会跟着快照一直存下去（state.json 里能看到孤儿的 svcRes 键）。
func TestServiceResultDroppedForDeletedService(t *testing.T) {
	st := New(t.TempDir())
	if !st.CreateService(&model.Service{ID: "s1", Name: "待删", Type: "icmp", Target: "1.1.1.1", Interval: 60, Enabled: true}) {
		t.Fatal("建服务失败")
	}
	now := time.Now().Unix()

	st.AddServiceResult(model.ServiceResult{ServiceID: "s1", TS: now, OK: true, LatencyMs: 5})
	if got := len(st.ServiceSeries("s1")); got != 1 {
		t.Fatalf("正常结果应入库，实际 %d 个点", got)
	}

	if !st.DeleteService("s1") {
		t.Fatal("删服务失败")
	}
	// 迟到的结果：服务已经不在了
	st.AddServiceResult(model.ServiceResult{ServiceID: "s1", TS: now + 1, OK: true, LatencyMs: 5})
	if got := len(st.ServiceSeries("s1")); got != 0 {
		t.Fatalf("已删服务的迟到结果不该重建序列，实际 %d 个点", got)
	}
}

// TestOrphanServiceSeriesDroppedOnLoad 覆盖"历史里已有的孤儿序列"。
//
// 已经在 state.json 里的孤儿数据不会因为修了写入路径就消失，所以加载快照时
// 也要按现存服务过滤一次 —— 否则旧数据会一直跟着快照走，永远不会被清掉。
func TestOrphanServiceSeriesDroppedOnLoad(t *testing.T) {
	dir := t.TempDir()
	st := New(dir)
	st.CreateService(&model.Service{ID: "live", Name: "活着的", Type: "icmp", Target: "1.1.1.1", Interval: 60, Enabled: true})
	now := time.Now().Unix()
	st.AddServiceResult(model.ServiceResult{ServiceID: "live", TS: now, OK: true, LatencyMs: 3})
	// 手工塞一个孤儿：模拟"修好之前就已经落盘"的脏数据
	st.mu.Lock()
	r := NewServiceRing(ServiceRingSize)
	r.Add(model.ServiceSample{TS: now, LatencyMs: 1, OK: true, CertDays: -1})
	st.svcRes["ghost"] = r
	st.mu.Unlock()
	if err := st.Save(); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	// 重新加载：孤儿应被丢弃，正常序列保留
	st2 := New(dir)
	if err := st2.load(); err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if got := len(st2.ServiceSeries("ghost")); got != 0 {
		t.Fatalf("孤儿序列应在加载时被丢弃，实际 %d 个点", got)
	}
	if got := len(st2.ServiceSeries("live")); got != 1 {
		t.Fatalf("正常序列应保留，实际 %d 个点", got)
	}
}

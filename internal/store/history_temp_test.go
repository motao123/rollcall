package store

import (
	"testing"

	"github.com/motao123/rollcall/internal/model"
)

// TestHistoryTempBucket 覆盖温度的聚合语义：
// 桶内取**最高温**（max）而不是均值 —— 告警关心"最热的那一点"，
// 均值会把一次 15 秒的风扇停转抹平；无温度的样本不影响已记录的值，
// 全程无温度的节点桶值为 0（前端据此整块不渲染）。
func TestHistoryTempBucket(t *testing.T) {
	s := New(t.TempDir())
	base := histBase
	// 同一分钟内的 4 个样本：30 / 55 / 88 / 71 → 桶内最高 88
	temps := []float64{30, 55, 88, 71}
	for i, tc := range temps {
		s.AddMetrics("n1", model.HostInfo{}, model.Metrics{
			TS:     base + int64(i)*15,
			CPUPct: 10, TempC: tc,
		})
	}

	m1 := s.HistorySeries("n1", HistoryResolution1m, base)
	if len(m1) != 1 {
		t.Fatalf("应聚成 1 个 1 分钟桶，实际 %d 个", len(m1))
	}
	if m1[0].TempC != 88 {
		t.Fatalf("桶内温度应取最高值 88，实际 %.1f", m1[0].TempC)
	}
}

// TestHistoryTempAbsentIsZero 确认无传感器的节点桶值为 0，
// 且**不会**被同桶内其他字段的存在伪造出来。
func TestHistoryTempAbsentIsZero(t *testing.T) {
	s := New(t.TempDir())
	for i := 0; i < 3; i++ {
		s.AddMetrics("n1", model.HostInfo{}, model.Metrics{
			TS: histBase + int64(i)*15, CPUPct: 10,
		})
	}
	m1 := s.HistorySeries("n1", HistoryResolution1m, histBase)
	if len(m1) != 1 || m1[0].TempC != 0 {
		t.Fatalf("无温度数据时桶内应为 0，实际 %+v", m1)
	}
}

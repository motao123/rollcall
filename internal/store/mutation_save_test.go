package store

import (
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/model"
)

// TestDeleteServiceRequestsSave 防止删除只改内存不落盘，重启后服务「复活」。
func TestDeleteServiceRequestsSave(t *testing.T) {
	st := New(t.TempDir())
	st.UpsertService(&model.Service{
		ID: "s1", NodeID: "n1", Name: "演示", Type: "http",
		Target: "https://example.com", Interval: 60, Enabled: true,
	})
	// 清掉 Upsert 产生的落盘请求（缓冲为 1）
	select {
	case <-st.saveReq:
	default:
	}

	if !st.DeleteService("s1") {
		t.Fatal("应删除成功")
	}
	select {
	case <-st.saveReq:
	case <-time.After(time.Second):
		t.Fatal("DeleteService 未请求落盘")
	}
}

// TestSatMulDoesNotWrap 防止历史回填的 uint64 乘法回绕成极小值。
func TestSatMulDoesNotWrap(t *testing.T) {
	if got := satMul(1<<62, 8); got != ^uint64(0) {
		t.Fatalf("溢出应饱和为最大值，实际 %d", got)
	}
	if got := satMul(100, 3); got != 300 {
		t.Fatalf("正常乘法 = %d，期望 300", got)
	}
	if got := satMul(0, 5); got != 0 {
		t.Fatalf("零乘应得 0，实际 %d", got)
	}
}

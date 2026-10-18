package store

import (
	"testing"
	"time"

	"github.com/motao/rollcall/internal/model"
)

// TestUpsertPathsRequestSave 是回归测试。
//
// 删除路径都调了 RequestSave，新增/修改路径却只靠 60 秒自动快照：
// 这个窗口内崩溃/重启会丢掉刚建的节点**连同它的上报令牌**，
// 该 Agent 重启后直接 401，且无法从日志看出原因。
func TestUpsertPathsRequestSave(t *testing.T) {
	st := New(t.TempDir())
	drainSave(st)
	st.UpsertNode(&model.Node{ID: "n1", Name: "n1", Token: "tok"})
	waitSave(t, st, "UpsertNode")

	drainSave(st)
	st.UpsertService(&model.Service{ID: "s1", NodeID: "n1", Type: "http", Target: "https://example.com"})
	waitSave(t, st, "UpsertService")

	drainSave(st)
	st.UpsertRule(&model.AlertRule{ID: "r1", Name: "规则", Metric: "cpu", Op: "gt", Threshold: 90})
	waitSave(t, st, "UpsertRule")
}

// TestNotifyJobTerminalTimestamps 是回归测试。
//
// 失败与静默任务原来不写任何时间戳（只有成功写 SentAt），
// 面板无法回答「什么时候失败的」，而调用方一直在传这个时间。
func TestNotifyJobTerminalTimestamps(t *testing.T) {
	st := New(t.TempDir())
	now := time.Now().Unix()
	j := &model.NotifyJob{ID: "j1", RuleID: "r1", NodeID: "n1", Status: model.NotifyPending}
	if !st.EnqueueNotifyJob(j) {
		t.Fatal("入队失败")
	}
	if !st.NotifyJobFailed("j1", "boom", now) {
		t.Fatal("标记失败失败")
	}
	jobs := st.NotifyJobs(10)
	if len(jobs) != 1 {
		t.Fatalf("任务数 = %d", len(jobs))
	}
	if jobs[0].FinishedAt != now {
		t.Fatalf("失败任务缺少完成时间: %+v", jobs[0])
	}

	j2 := &model.NotifyJob{ID: "j2", RuleID: "r1", NodeID: "n1", Status: model.NotifyPending}
	if !st.EnqueueNotifyJob(j2) {
		t.Fatal("入队失败")
	}
	if !st.NotifyJobSuppressed("j2", "global", "静默中", now) {
		t.Fatal("标记静默失败")
	}
	for _, job := range st.NotifyJobs(10) {
		if job.ID == "j2" && job.FinishedAt != now {
			t.Fatalf("静默任务缺少完成时间: %+v", job)
		}
	}
}

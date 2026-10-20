package store

import (
	"errors"
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/model"
)

func TestNotifyJobsListAndRetry(t *testing.T) {
	s := New(t.TempDir())
	now := time.Now().Unix()

	mk := func(id, status string, created int64) *model.NotifyJob {
		return &model.NotifyJob{
			ID: id, RuleID: "r1", Title: id, Status: status,
			CreatedAt: created, Attempts: 5, LastError: "boom", SuppressedBy: "r1",
		}
	}
	jobs := []*model.NotifyJob{
		mk("nj_sent", model.NotifySent, now-40),
		mk("nj_pending", model.NotifyPending, now-30),
		mk("nj_suppressed", model.NotifySuppressed, now-20),
		mk("nj_failed", model.NotifyFailed, now-10),
	}
	for _, j := range jobs {
		if !s.EnqueueNotifyJob(j) {
			t.Fatalf("入队 %s 失败", j.ID)
		}
	}

	got := s.NotifyJobs(10)
	want := []string{"nj_pending", "nj_failed", "nj_suppressed", "nj_sent"}
	if len(got) != len(want) {
		t.Fatalf("NotifyJobs 返回 %d 条，期望 %d", len(got), len(want))
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Fatalf("第 %d 条为 %s，期望 %s（未完成/失败应优先）", i, got[i].ID, id)
		}
	}
	if limited := s.NotifyJobs(2); len(limited) != 2 {
		t.Fatalf("limit=2 返回 %d 条", len(limited))
	}

	if err := s.RetryNotifyJob("nj_failed", now); err != nil {
		t.Fatalf("重试失败任务: %v", err)
	}
	var retried *model.NotifyJob
	for _, j := range s.NotifyJobs(10) {
		if j.ID == "nj_failed" {
			retried = j
		}
	}
	if retried == nil {
		t.Fatal("重试后任务丢失")
	}
	if retried.Status != model.NotifyPending || retried.Attempts != 0 ||
		retried.LastError != "" || retried.SuppressedBy != "" || retried.NextAttemptAt != now {
		t.Fatalf("重试后任务状态未重置: %+v", retried)
	}

	if err := s.RetryNotifyJob("nj_failed", now); !errors.Is(err, ErrNotifyJobPending) {
		t.Fatalf("重复重试应返回 ErrNotifyJobPending，实际 %v", err)
	}
	if err := s.RetryNotifyJob("nj_sent", now); !errors.Is(err, ErrNotifyJobSent) {
		t.Fatalf("已送达任务重试应返回 ErrNotifyJobSent，实际 %v", err)
	}
	if err := s.RetryNotifyJob("nj_missing", now); !errors.Is(err, ErrNotifyJobNotFound) {
		t.Fatalf("不存在的任务应返回 ErrNotifyJobNotFound，实际 %v", err)
	}
}

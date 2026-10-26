package main

import (
	"testing"
	"time"
)

// TestApplyTimezone 覆盖有效/空/非法三种输入。
//
// 非法时区必须报错而不是静默回落到 Local：用户以为改了时区、
// 实际日报还在 UTC 触发，是最难发现的一类问题。
func TestApplyTimezone(t *testing.T) {
	orig := time.Local
	defer func() { time.Local = orig }()

	if got, err := applyTimezone("Asia/Shanghai"); err != nil {
		t.Fatalf("有效时区应成功: %v", err)
	} else if got != "Asia/Shanghai" {
		t.Fatalf("生效值应为 Asia/Shanghai，实际 %q", got)
	}
	// 生效后按该时区计算偏移：8 月上海为 UTC+8
	if _, off := time.Date(2026, 8, 1, 12, 0, 0, 0, time.Local).Zone(); off != 8*3600 {
		t.Fatalf("上海应为 UTC+8，实际偏移 %d", off)
	}

	// 空值：不改变当前设置，返回当前值
	time.Local = orig
	before := time.Local.String()
	if got, err := applyTimezone("  "); err != nil || got != before {
		t.Fatalf("空值应保持原状: got=%q err=%v want=%q", got, err, before)
	}

	// 非法值：必须报错，且不能把 time.Local 改坏
	time.Local = orig
	if _, err := applyTimezone("Mars/Olympus"); err == nil {
		t.Fatal("非法时区必须报错")
	}
	if time.Local != orig {
		t.Fatal("非法输入不应修改 time.Local")
	}
}

package logins

import (
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/model"
)

// mkRecord 造一条 384 字节的 utmp 记录，用于解析测试。
// 字段偏移与 glibc 的 struct utmp 一致（见 logins.go 顶部常量）。
func mkRecord(typ int16, user, line, host string, ts int32, addrV4 []byte) []byte {
	rec := make([]byte, recordSize)
	binary.LittleEndian.PutUint16(rec[offType:], uint16(typ))
	copy(rec[offLine:offLine+32], line)
	copy(rec[offUser:offUser+32], user)
	copy(rec[offHost:offHost+256], host)
	binary.LittleEndian.PutUint32(rec[offTime:], uint32(ts))
	if len(addrV4) == 4 {
		copy(rec[offAddr:offAddr+4], addrV4)
	}
	return rec
}

// TestParseRecordsFiltersNonLogins 只保留 USER_PROCESS 且有用户名的记录。
//
// wtmp 里混着 BOOT_TIME、RUN_LVL、DEAD_PROCESS 等大量非登录记录，
// 不过滤就会把"系统启动"报成"有人登录"。
func TestParseRecordsFiltersNonLogins(t *testing.T) {
	var buf []byte
	buf = append(buf, mkRecord(2 /*BOOT_TIME*/, "reboot", "~", "", 1000, nil)...)
	buf = append(buf, mkRecord(typeUserProcess, "root", "pts/0", "203.0.113.9", 1001, nil)...)
	buf = append(buf, mkRecord(typeUserProcess, "", "pts/1", "203.0.113.10", 1002, nil)...)   // 无用户
	buf = append(buf, mkRecord(typeUserProcess, "ghost", "pts/2", "203.0.113.11", 0, nil)...) // 时间戳非法
	buf = append(buf, mkRecord(8 /*DEAD_PROCESS*/, "root", "pts/0", "", 1003, nil)...)
	buf = append(buf, mkRecord(typeUserProcess, "deploy", "pts/3", "host.example.com", 1004, nil)...)

	evs := parseRecords(buf)
	if len(evs) != 2 {
		t.Fatalf("应只保留 2 条登录，实际 %d: %+v", len(evs), evs)
	}
	if evs[0].User != "root" || evs[0].Host != "203.0.113.9" || evs[0].Line != "pts/0" || evs[0].TS != 1001 {
		t.Fatalf("第一条解析错误: %+v", evs[0])
	}
	if evs[1].User != "deploy" || evs[1].Host != "host.example.com" {
		t.Fatalf("第二条解析错误: %+v", evs[1])
	}
}

// TestParseRecordsFallsBackToAddr 老登录只写 ut_addr_v6 时用地址兜底。
func TestParseRecordsFallsBackToAddr(t *testing.T) {
	buf := mkRecord(typeUserProcess, "root", "tty1", "", 2000, []byte{198, 51, 100, 7})
	evs := parseRecords(buf)
	if len(evs) != 1 {
		t.Fatalf("应有 1 条，实际 %d", len(evs))
	}
	if evs[0].Host != "198.51.100.7" {
		t.Fatalf("应从 ut_addr_v6 兜底出 IPv4，实际 %q", evs[0].Host)
	}

	// 全零地址表示"没记录来源"（本地控制台），应保持为空而不是 0.0.0.0
	buf = mkRecord(typeUserProcess, "root", "tty2", "", 2001, []byte{0, 0, 0, 0})
	evs = parseRecords(buf)
	if len(evs) != 1 || evs[0].Host != "" {
		t.Fatalf("全零地址应留空，实际 %+v", evs)
	}
}

// TestParseRecordsIgnoresPartialTail 尾部半条记录不能被当成数据。
func TestParseRecordsIgnoresPartialTail(t *testing.T) {
	buf := mkRecord(typeUserProcess, "root", "pts/0", "203.0.113.9", 1001, nil)
	buf = append(buf, buf[:100]...) // 追加一条被截断的记录
	evs := parseRecords(buf)
	if len(evs) != 1 {
		t.Fatalf("半条记录应被忽略，实际 %d 条", len(evs))
	}
}

// TestWatcherFirstPollSilentThenIncremental 首次只建水位，之后只报增量。
//
// 首次就把 wtmp 里的历史全报出来，等于每次 Agent 重启都推一堆几个月前的
// 登录给运维 —— 这条用例把"首次静默"钉住。
func TestWatcherFirstPollSilentThenIncremental(t *testing.T) {
	w := NewWatcher()
	old := []model.LoginEvent{{TS: 100, User: "root", Host: "a"}, {TS: 200, User: "root", Host: "b"}}
	if got := w.absorb(old); got != nil {
		t.Fatalf("首次调用应只建水位，实际返回 %+v", got)
	}

	// 同样的历史再来一遍：仍然没有新事件
	if got := w.absorb(old); got != nil {
		t.Fatalf("没有新登录时不该上报，实际 %+v", got)
	}

	// 出现新登录（且旧记录还在窗口里）：只报新的那条
	evs := []model.LoginEvent{{TS: 200, User: "root", Host: "b"}, {TS: 300, User: "deploy", Host: "c"}}
	got := w.absorb(evs)
	if len(got) != 1 || got[0].TS != 300 || got[0].User != "deploy" {
		t.Fatalf("应只报 ts=300 那条，实际 %+v", got)
	}

	// 再报一次相同的：没有新事件（水位已推进）
	if got := w.absorb(evs); got != nil {
		t.Fatalf("重复的同一批不该再报，实际 %+v", got)
	}
}

// TestWatcherCapsBatch 单次上报最多 20 条，且保留最新的。
func TestWatcherCapsBatch(t *testing.T) {
	w := NewWatcher()
	w.absorb([]model.LoginEvent{{TS: 1, User: "x", Host: "h"}}) // 建水位

	var burst []model.LoginEvent
	for i := 2; i <= 30; i++ {
		burst = append(burst, model.LoginEvent{TS: int64(i), User: "u", Host: "h"})
	}
	got := w.absorb(burst)
	if len(got) != maxEventPerReport {
		t.Fatalf("应截到 %d 条，实际 %d", maxEventPerReport, len(got))
	}
	if got[len(got)-1].TS != 30 {
		t.Fatalf("应保留最新的一条（ts=30），实际 %d", got[len(got)-1].TS)
	}
	if got[0].TS != 30-int64(maxEventPerReport)+1 {
		t.Fatalf("应保留最新的 %d 条，首条 ts=%d", maxEventPerReport, got[0].TS)
	}
}

// TestParseRecordsTrimsPadding utmp 的定长字段用 NUL 填充，必须裁掉。
func TestParseRecordsTrimsPadding(t *testing.T) {
	rec := mkRecord(typeUserProcess, "root", "pts/0", "1.2.3.4", 100, nil)
	// 模拟字段后面残留空格（有些实现会这么写）
	copy(rec[offUser:offUser+32], "root    ")
	evs := parseRecords(rec)
	if len(evs) != 1 || strings.TrimSpace(evs[0].User) != "root" || evs[0].User != "root" {
		t.Fatalf("用户名应被裁掉尾部空格，实际 %q", evs[0].User)
	}
}

// TestClassifyLoginState 覆盖采集状态的判定阈值。
//
// 这条决定了面板显示"正常"还是"未在写入"：判错方向会让一个永远不触发的
// 功能看起来是正常的（更糟的是反过来，把正常的机器标成坏了）。
func TestClassifyLoginState(t *testing.T) {
	cases := []struct {
		age  time.Duration
		want string
	}{
		{0, StateWtmp},
		{time.Hour, StateWtmp},
		{6 * 24 * time.Hour, StateWtmp},
		{8 * 24 * time.Hour, StateWtmpStale},
		{365 * 24 * time.Hour, StateWtmpStale},
	}
	for _, c := range cases {
		if got := classify(c.age); got != c.want {
			t.Fatalf("age=%v 应判为 %s，实际 %s", c.age, c.want, got)
		}
	}
}

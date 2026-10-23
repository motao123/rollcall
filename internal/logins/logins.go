// Package logins 采集节点的交互式登录事件（SSH/控制台）。
//
// 数据源是 /var/log/wtmp：内核与 login/sshd 在**成功**登录后才写入一条
// USER_PROCESS 记录，失败尝试不会进来。因此这里拿到的是"确实有人进来了"，
// 而不是"有人在试"——后者要靠 fail2ban 或 auth.log，不在本包范围。
//
// 设计约束与 Agent 的整体原则一致：只读文件、不改任何东西、不执行命令。
// 解析是纯粹的内存操作（parseRecords），读文件与水位（watermark）分开，
// 这样格式解析能在任何平台上被单元测试覆盖。
package logins

import (
	"encoding/binary"
	"fmt"
	"strings"
	"time"

	"github.com/motao123/rollcall/internal/model"
)

// utmp 记录在 Linux 上是固定 384 字节（glibc 的 struct utmp，32/64 位一致）。
// 各字段是固定宽度，所以不需要按架构分支。
const (
	recordSize = 384
	// USER_PROCESS 表示"一个用户会话已经开始"，是唯一需要的类型。
	// 同一个登录还会伴随 DEAD_PROCESS（登出）等记录，那些不报。
	typeUserProcess = 7

	offType = 0
	offLine = 8   // char[32]
	offUser = 44  // char[32]
	offHost = 76  // char[256]
	offTime = 340 // struct timeval{int32 tv_sec; int32 tv_usec}
	offAddr = 348 // int32 ut_addr_v6[4]
)

// maxEventPerReport 限制单次上报携带的登录事件数量。
//
// 它同时是"运维可见性"与"载荷上限"的折中：真的有人在一个上报周期里登录
// 20 次以上，要么是在批量操作，要么是被入侵后建了多个会话 —— 两种情况都
// 不至于因为第 21 条被丢掉而失去判断依据。
const maxEventPerReport = 20

// parseRecords 把一段 wtmp 原始字节解析成登录事件。
//
// 只保留 USER_PROCESS；用户名为空、时间戳非正的记录一律丢弃（utmp 里
// 有大量 RUN_LVL/BOOT_TIME 之类的非登录记录，以及被截断的尾部垃圾）。
func parseRecords(b []byte) []model.LoginEvent {
	out := make([]model.LoginEvent, 0, len(b)/recordSize)
	for off := 0; off+recordSize <= len(b); off += recordSize {
		rec := b[off : off+recordSize]
		if int16(binary.LittleEndian.Uint16(rec[offType:])) != typeUserProcess {
			continue
		}
		user := cstr(rec[offUser : offUser+32])
		if user == "" {
			continue
		}
		ts := int64(int32(binary.LittleEndian.Uint32(rec[offTime:])))
		if ts <= 0 {
			continue
		}
		host := cstr(rec[offHost : offHost+256])
		if host == "" {
			// 老的 telnet/控制台登录只写 ut_addr_v6；前 4 字节是 IPv4。
			// 全零表示"没记地址"，此时留空由上层展示为"本地"。
			ip := rec[offAddr : offAddr+4]
			if ip[0]|ip[1]|ip[2]|ip[3] != 0 {
				host = formatIPv4(ip)
			}
		}
		out = append(out, model.LoginEvent{
			TS:   ts,
			User: user,
			Host: host,
			Line: cstr(rec[offLine : offLine+32]),
		})
	}
	return out
}

// cstr 读取定长 C 字符串（到 NUL 或字段末尾）。
func cstr(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		b = b[:i]
	}
	return strings.TrimRight(string(b), "\x00 ")
}

// formatIPv4 把 ut_addr_v6 的前 4 字节格式化成点分十进制。
// 其余 12 字节在 IPv4 登录时全为 0，不参与展示。
func formatIPv4(b []byte) string {
	return fmt.Sprintf("%d.%d.%d.%d", b[0], b[1], b[2], b[3])
}

// Watcher 记住"已经报过哪一条"，只返回新出现的登录。
//
// 水位用时间戳而不是文件偏移：wtmp 会被 logrotate 轮转，偏移在轮转后指向
// 完全不同的位置；时间戳在轮转前后仍然可比。
type Watcher struct {
	wm      int64
	started bool

	// 最近一次采集的可用性，供上报时带给服务端。
	// 之所以要报这个：wtmp 在裁剪过的镜像上可能根本不写（缺 /run/utmp、
	// PAM 未配 utmp），此时功能会静默不触发 —— 那比"没有这个功能"更糟，
	// 因为运维以为有人在盯着。把状态报上去，面板就能显示"最近写入 N 天前"。
	note string
	age  int64
}

// 采集状态取值（随请求头上报，服务端只做展示）。
const (
	StateWtmp        = "wtmp"        // 可读，且最近有写入
	StateWtmpStale   = "wtmp-stale"  // 可读，但很久没写过（大概率 sshd 没在写）
	StateUnavailable = "unavailable" // 读不到
)

// LoginState 返回最近一次 Poll 观察到的采集状态与 wtmp 的最后写入时间（秒）。
func (w *Watcher) LoginState() (string, int64) { return w.note, w.age }

func NewWatcher() *Watcher { return &Watcher{} }

// Poll 返回自上次调用以来的新登录事件（按时间升序）。
//
// 首次调用**只建立水位、不返回任何事件**：Agent 刚装好或刚重启时，wtmp 里
// 可能存着几个月前的登录记录，把一整批历史当成"新登录"推给运维是纯噪音。
// 代价是 Agent 重启前几秒内的登录可能被漏掉 —— 相比每天几条误报，这个
// 取舍更划算，也在文档里写明了。
func (w *Watcher) Poll() []model.LoginEvent {
	recs, age, err := readTail()
	if err != nil {
		w.note, w.age = StateUnavailable, -1
		return nil
	}
	w.age = int64(age.Seconds())
	w.note = classify(age)
	if len(recs) == 0 {
		return nil
	}
	return w.absorb(parseRecords(recs))
}

// staleAfter 是判定"wtmp 没在写"的阈值。
const staleAfter = 7 * 24 * time.Hour

// classify 把 wtmp 的最后写入年龄映射成采集状态。
//
// 正常节点每次登录/开关机都会写一条记录；一周没写基本可以断定写不进去
// （缺 /run/utmp、PAM 没配 utmp 之类），而不是"恰好一周没人登录"。
func classify(age time.Duration) string {
	if age > staleAfter {
		return StateWtmpStale
	}
	return StateWtmp
}

// absorb 处理一批已解析的事件，返回需要上报的新事件。
//
// 与 readTail 分开是为了可测：水位逻辑在任何平台都能被单元测试覆盖，
// 而读文件那部分只能在 Linux 上真实跑。
func (w *Watcher) absorb(evs []model.LoginEvent) []model.LoginEvent {
	if len(evs) == 0 {
		return nil
	}

	// 找到本轮最大时间戳，作为下一轮的水位。
	var newest int64
	for _, e := range evs {
		if e.TS > newest {
			newest = e.TS
		}
	}
	if !w.started {
		w.started = true
		w.wm = newest
		return nil
	}
	if newest <= w.wm {
		return nil
	}

	out := make([]model.LoginEvent, 0, 4)
	for _, e := range evs {
		if e.TS > w.wm {
			out = append(out, e)
		}
	}
	w.wm = newest
	if len(out) > maxEventPerReport {
		// 保留最新的那批：突发登录时最近的更可能相关。
		out = out[len(out)-maxEventPerReport:]
	}
	return out
}

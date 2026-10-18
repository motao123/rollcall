// Package agentwire 定义 Agent 与服务端共同依赖的上报契约常量。
//
// 这些量两侧都必须用同一份：服务端按 OnlineTTL 判定离线，Agent 按它推出
// 自己的上报节奏。若两侧各持一份，改了一边忘另一边，表现是"配置看起来
// 合法、实际必然误报失联"——这类 bug 只在网络抖动时才暴露，很难复现。
//
// 更重要的理由：服务端是被攻破后危害最大的一端。上报节奏的上限由 Agent
// 本地按这里的常量再夹一次，服务端即使被控，也不能把节点改成一个每 100
// 毫秒上报一次的放大器。缺失一侧的校验，就只剩另一侧在守。
package agentwire

// OnlineTTL 是判定离线的时长：最后一次成功上报之后经过这么久仍未收到新
// 上报，即视为离线。它是"是否在线"的唯一来源——不存在第二张在线表，也
// 没有连接状态可以与它分叉。
const OnlineTTL = 120 // 秒

const (
	// MinOnlineTTL / MaxOnlineTTL 是 OnlineTTL 的合法区间。
	MinOnlineTTL = 30
	MaxOnlineTTL = 1800
)

// ReportsPerTTL 是一个 TTL 内的上报机会数：容得下 ReportsPerTTL-1 次连续
// 失败。取 4 意味着 120 秒的 TTL 里有一次网络抖动、一次采集超时、一次
// 重试都还能赶在下一次判定之前。
const ReportsPerTTL = 4

// MinReportInterval 是允许配置的最小上报间隔（秒）。
// 再密就只是在给主控加压，且 40 台机器同时放大时会打满主控的连接数。
const MinReportInterval = 5

// MaxResponseBytes 是 Agent 读取服务端响应正文的上限。
// 服务端不该对一次上报返回大包；Agent 侧硬夹一次，避免被诱导下载大文件。
const MaxResponseBytes = 64 << 10

// ReportInterval 是推荐上报间隔的唯一算法。两侧都从它推出，而不是各写一个数。
func ReportInterval(ttl int) int {
	if ttl <= 0 {
		ttl = OnlineTTL
	}
	return ttl / ReportsPerTTL
}

// MaxReportInterval 是本地配置允许放宽到的上限：不超过 TTL 的一半，
// 保证一个 TTL 内至少有两次上报机会，单次丢包不会误判离线。
func MaxReportInterval(ttl int) int {
	if ttl <= 0 {
		ttl = OnlineTTL
	}
	return ttl / 2
}

// ClampReportInterval 把配置值夹进 [MinReportInterval, MaxReportInterval]，
// 越界（含 0 与负数）取最近的边界。ok 为假表示原值越界。
func ClampReportInterval(sec, ttl int) (out int, ok bool) {
	lo, hi := MinReportInterval, MaxReportInterval(ttl)
	switch {
	case sec < lo:
		return lo, false
	case sec > hi:
		return hi, false
	}
	return sec, true
}

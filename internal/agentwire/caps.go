package agentwire

import (
	"sort"
	"strings"
)

// HeaderAgentCaps 是 Agent 自报"可选采集能力"的请求头。
//
// # 为什么用请求头而不是上报 JSON 字段
//
// 服务端用 DisallowUnknownFields 解码上报体，给 body 加字段会让**旧服务端**
// 把整份上报拒掉（升级顺序被迫反转）。请求头对旧服务端完全透明，
// 与 X-Agent-Version / X-NIC-Set / X-Login-* 是同一套做法。
//
// # 为什么需要这个头
//
// 温度、磁盘 IO 这类采集项依赖机器本身是否具备条件（例如 hwmon 传感器）。
// 没有这个头时，面板看到"没有温度数据"无法区分三种情况：
//
//  1. 这台机器没有传感器      → 永久不会有数据，属于正常
//  2. Agent 读不到（权限/内核）→ 需要排查
//  3. Agent 版本太旧，压根没采  → 升级即可
//
// 三者混在一个"空"里，界面就只能沉默。能力上报把 1 与 2/3 分开。
const HeaderAgentCaps = "X-Agent-Caps"

// CapsNone 是"已确认没有任何可选能力"的哨兵值。
//
// 必须与"头缺席"区分开：缺席 = 老 Agent（未知），none = 新 Agent 已探测且确实没有。
// 少了这个哨兵，空清单会被当成"没发头"，三种情况又退回成一团。
const CapsNone = "none"

// maxCapsLen 是能力清单的长度上限（字节）。
// 能力是枚举值而不是自由文本；截断而不是拒绝，避免一个异常 Agent
// 用超长头把面板字段撑爆。
const maxCapsLen = 64

// NormalizeCaps 把能力列表规整为逗号分隔的小写串，去重并排序。
//
// 空列表返回 CapsNone 而不是空串：空串会被服务端当成"头缺席"。
func NormalizeCaps(list []string) string {
	seen := make(map[string]struct{}, len(list))
	out := make([]string, 0, len(list))
	for _, raw := range list {
		v := strings.ToLower(strings.TrimSpace(raw))
		if v == "" || v == CapsNone {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	if len(out) == 0 {
		return CapsNone
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// ParseCaps 解析能力头，返回规整后的清单与"是否已知"。
//
//	""        → ("", false)      头缺席：老 Agent，能力未知
//	"none"    → ("none", true)   已确认无能力
//	"Temp, x" → ("temp,x", true)
//
// 两端共用这一个函数：这类枚举字段最容易在 Agent 与服务端各写一套解析，
// 然后在对大小写、空项、重复项的处理上分叉。
func ParseCaps(header string) (caps string, known bool) {
	h := strings.TrimSpace(header)
	if h == "" {
		return "", false
	}
	if len(h) > maxCapsLen {
		h = h[:maxCapsLen]
	}
	return NormalizeCaps(strings.Split(h, ",")), true
}

// HasCaps 判断规整后的能力串里是否包含某项能力。
func HasCaps(caps, want string) bool {
	if caps == "" || caps == CapsNone || want == "" {
		return false
	}
	for _, v := range strings.Split(caps, ",") {
		if v == want {
			return true
		}
	}
	return false
}

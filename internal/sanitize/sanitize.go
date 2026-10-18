// Package sanitize 清洗会进入日志、终端与页面的外来字符串。
//
// Agent 是可能被攻破的一端：它自报的主机名、系统版本、CPU 型号会原样进入
// 内存快照、状态页、CSV 导出、Prometheus 输出与服务端日志。这些字符串因此
// 必须当成不可信输入处理，而不是"从我们自己机器上读出来的就可信"。
package sanitize

import (
	"strings"
	"unicode/utf8"
)

// IsControl 报告该字符是否需要剔除。
//
// 四类一并处理：
//   - C0（0x00–0x1F）与 DEL：能伪造日志换行，把一条记录伪装成两条；
//   - C1（0x80–0x9F）：终端控制序列；
//   - 双向控制符（U+202A–U+202E、U+2066–U+2069、U+200E/U+200F）：能让一段
//     文本在编辑器、终端和日志里显示成与真实字节序不同的样子（Trojan Source
//     一类手法）。监控系统的告警正是靠人读这些文本做判断，必须剔除；
//   - U+2028/U+2029：在 JSON 字符串里合法，却是 JavaScript 的行终止符，
//     进入页面脚本上下文会截断字符串。
//
// 其他格式字符（ZWNJ U+200C、ZWJ U+200D 等）保留：它们是波斯语、印地语等
// 文字里正常词的一部分，剔除会毁掉这些文字的正常显示。
func IsControl(r rune) bool {
	switch {
	case r < 0x20: // C0
		return true
	case r == 0x7f: // DEL
		return true
	case r >= 0x80 && r <= 0x9f: // C1
		return true
	case r >= 0x202a && r <= 0x202e: // LRE RLE PDF LRO RLO
		return true
	case r >= 0x2066 && r <= 0x2069: // LRI RLI FSI PDI
		return true
	case r == 0x200e || r == 0x200f: // LRM RLM
		return true
	case r == 0x2028 || r == 0x2029: // LS PS
		return true
	}
	return false
}

// String 剔除控制字符与双向控制符，并在 maxBytes 处按完整字符截断：
// 截断落在 rune 边界上，不会产出半个字符导致下游 UTF-8 解码失败。
// maxBytes <= 0 表示不截断。
func String(s string, maxBytes int) string {
	if !strings.ContainsFunc(s, IsControl) && (maxBytes <= 0 || len(s) <= maxBytes) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if IsControl(r) {
			continue
		}
		if maxBytes > 0 && b.Len()+utf8.RuneLen(r) > maxBytes {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Text 是"要显示成一行文字"的外来字符串的共同口径：先 String 再裁首尾空白。
//
// 顺序不能反：先裁空白的话，剔除控制字符后会露出新的首尾空白。
// 截断发生在裁空白之前，所以结果可能短于 maxBytes。
func Text(s string, maxBytes int) string {
	return strings.TrimSpace(String(s, maxBytes))
}

// HasControl 报告字符串里是否含有需要清洗的字符或非法 UTF-8，供入口做
// "拒绝"判定（例如节点名、分组名这类应当显式拒绝、而不是悄悄改写的字段）。
//
// 与 String 共用 IsControl，两处对"控制字符"是同一个口径：一处剔除、
// 一处拒绝，不会出现"这里算控制字符、那里不算"的分歧。
func HasControl(s string) bool {
	if !utf8.ValidString(s) {
		return true
	}
	return strings.ContainsFunc(s, IsControl)
}

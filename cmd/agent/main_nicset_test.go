package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/motao123/rollcall/internal/model"
)

// TestNICSetFingerprint 指纹只取决于网卡名集合（与顺序、计数器值无关）。
func TestNICSetFingerprint(t *testing.T) {
	a := nicSetFingerprint([]model.NICInfo{{Name: "eth0"}, {Name: "eth1"}})
	b := nicSetFingerprint([]model.NICInfo{{Name: "eth1", Rx: 999}, {Name: "eth0", Rx: 1}})
	if a == "" || a != b {
		t.Fatalf("同一名单不同顺序/读数应产生同一指纹: %s vs %s", a, b)
	}
	c := nicSetFingerprint([]model.NICInfo{{Name: "eth0"}, {Name: "eth1"}, {Name: "wg0"}})
	if c == a {
		t.Fatal("新增网卡应改变指纹")
	}
	if got := nicSetFingerprint(nil); got != "" {
		t.Fatalf("空集合应返回空串，实际 %q", got)
	}
	if len(a) != 16 || strings.ContainsAny(a, "ghijklmnopqrstuvwxyz") {
		t.Fatalf("指纹应为 16 位小写十六进制，实际 %q", a)
	}
}

// TestNICSetFingerprintCoversBeyondTop8 是 A1 的回归测试：
// 指纹必须覆盖**参与求和的完整集合**。早先实现把明细截到前 8 块再算指纹，
// 超过 8 块网卡的机器上，第 9 块的增删改变了求和口径、指纹却纹丝不动，
// 那次流量跳变就被记成了真实用量。
//
// 修复后指纹在截断之前计算 —— 这条测试钉住"第 9 块的变化必须可见"。
func TestNICSetFingerprintCoversBeyondTop8(t *testing.T) {
	base := make([]model.NICInfo, 0, 9)
	for i := 0; i < 8; i++ {
		base = append(base, model.NICInfo{Name: fmt.Sprintf("eth%d", i)})
	}
	// 只有前 8 块时的指纹
	fp8 := nicSetFingerprint(base)
	// 加入第 9 块（正好是被旧实现截掉的位置）
	with9 := append(append([]model.NICInfo(nil), base...), model.NICInfo{Name: "eth8"})
	fp9 := nicSetFingerprint(with9)
	if fp8 == fp9 {
		t.Fatal("第 9 块网卡的增删必须改变指纹（指纹不能在截断之后计算）")
	}
	// 换掉第 9 块的名字同样要可见
	with9b := append(append([]model.NICInfo(nil), base...), model.NICInfo{Name: "wg9"})
	if fp9 == nicSetFingerprint(with9b) {
		t.Fatal("截断线以下的名字变化也应改变指纹")
	}
	_ = strings.TrimSpace // 保持 strings 导入（原测试用到）
}

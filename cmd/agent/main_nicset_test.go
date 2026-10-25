package main

import (
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

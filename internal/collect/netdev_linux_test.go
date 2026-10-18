//go:build linux

package collect

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSkipIfaceAvoidsDoubleCounting 是回归测试。
//
// 只按名字过滤 lo/veth/docker 时，bond0 与其 slave（eth0/eth1）、
// 网桥与容器虚拟网卡会把同一批报文累加多次，累计流量放大 2–3 倍；
// 而历史层正是用这些累计值做差分算流量。
func TestSkipIfaceAvoidsDoubleCounting(t *testing.T) {
	root := t.TempDir()
	old := sysClassNet
	sysClassNet = root
	defer func() { sysClassNet = old }()

	mk := func(name string, master bool) {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if master {
			if err := os.Symlink("../../bond0", filepath.Join(dir, "master")); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("eth0", true) // bond 成员
	mk("bond0", false)
	mk("veth1a2b", false)
	mk("br-9f8e", false)

	cases := map[string]bool{
		"lo":       true,
		"eth0":     true, // 有 master：从设备
		"veth1a2b": true,
		"br-9f8e":  true,
		"bond0":    false, // 主设备：要统计
		"ens3":     false, // 普通物理网卡
	}
	for name, want := range cases {
		if got := skipIface(name); got != want {
			t.Errorf("skipIface(%q) = %v，期望 %v", name, got, want)
		}
	}
}

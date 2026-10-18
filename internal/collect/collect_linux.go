//go:build linux

// Package collect 实现只读指标采集。
//
// ★ 安全约束（本项目的核心设计）★
//
//	· 只读 /proc、/etc/os-release 与 statfs，不读取用户数据
//	· 不 import os/exec —— 无法执行任何命令
//	· 不 import net 的监听相关能力 —— 不开放任何端口
//	· 不做任何持久化（除自身配置文件）
//
// 验证方式（在项目根目录执行）：
//
//	# ① 依赖表里不应出现 os/exec —— 最可靠、最快的证明
//	go list -deps ./cmd/agent | grep -x 'os/exec'   # 期望：无输出
//
//	# ② 符号表检查：必须用**未剥离**的二进制！
//	#    Makefile 的 -ldflags "-s -w" 会剥掉符号表，
//	#    此时 go tool nm 报 "no symbol section" 且 grep 计数恒为 0，
//	#    看起来"通过"其实是假通过。
//	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/agent-full ./cmd/agent
//	go tool nm /tmp/agent-full | wc -l                                  # 期望：数千（证明符号表在）
//	go tool nm /tmp/agent-full | grep -cE 'os/exec\.|exec\.Cmd'         # 期望：0
//	go tool nm /tmp/agent-full | grep -cE 'net\.Listen|TCPListener'     # 期望：0
//
//	# ③ 对照组：server 二进制必须有 Listen，否则说明 grep 模式写错了
//	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/server-full ./cmd/server
//	go tool nm /tmp/server-full | grep -cE 'net\.Listen|TCPListener'    # 期望：>0
//
// ⚠ 不要用 grep 扫源码来"验证"：本注释自身就含有这些字符串，
// 直接 grep 必然命中，无法区分注释与真实调用。
package collect

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/motao/rollcall/internal/model"
)

// Collector 持有上一次 CPU 采样，用于计算区间使用率。
type Collector struct {
	cpuMu    sync.Mutex
	prevIdle uint64
	prevTot  uint64
	hostOnce sync.Once
	host     model.HostInfo
}

func New() *Collector { return &Collector{} }

// Host 返回静态硬件信息（仅首次真正读取）。
// 用 sync.Once 而非与 CPU 采样共用锁：Host 会读多个文件，
// 不应把文件 IO 串行到每次指标采集之前。
func (c *Collector) Host() model.HostInfo {
	c.hostOnce.Do(func() {
		h := model.HostInfo{}
		if hn, err := os.Hostname(); err == nil {
			h.Hostname = hn
		}
		h.Arch = runtime.GOARCH
		h.Cores = runtime.NumCPU()
		if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
			h.Kernel = strings.TrimSpace(string(b))
		}
		if b, err := os.ReadFile("/etc/os-release"); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(line, "PRETTY_NAME=") {
					h.OS = strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"`)
				}
			}
		}
		if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(line, "model name") {
					if i := strings.Index(line, ":"); i > 0 {
						h.CPUModel = strings.TrimSpace(line[i+1:])
					}
					break
				}
			}
		}
		// 虚拟化类型：KVM / VMware / 物理机
		h.Virt = detectVirt()
		c.host = h
	})
	return c.host
}

// detectVirt 判断虚拟化类型。
//
// 不能只看 /proc/cpuinfo 的 CPU 型号：KVM 直通/宿主机型号透传时
// cpuinfo 里通常不含 "KVM"（hypervisor vendor 在 CPUID 0x40000000，
// /proc/cpuinfo 不暴露），Hyper-V 也很少带 "Microsoft"。
// 按 systemd-detect-virt 的思路优先读 /sys 与 DMI。
func detectVirt() string {
	if b, err := os.ReadFile("/sys/hypervisor/type"); err == nil {
		switch v := strings.TrimSpace(string(b)); {
		case v != "":
			if len(v) > 1 {
				return strings.ToUpper(v[:1]) + v[1:]
			}
			return v
		}
	}
	for _, p := range []string{"/sys/class/dmi/id/product_name", "/sys/class/dmi/id/sys_vendor"} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		s := strings.ToLower(string(b))
		switch {
		case strings.Contains(s, "kvm"):
			return "KVM"
		case strings.Contains(s, "vmware"):
			return "VMware"
		case strings.Contains(s, "virtualbox"):
			return "VirtualBox"
		case strings.Contains(s, "hyper-v"), strings.Contains(s, "microsoft"):
			return "Hyper-V"
		case strings.Contains(s, "qemu"):
			return "QEMU"
		case strings.Contains(s, "xen"):
			return "Xen"
		}
	}
	// 最后退回 CPUID hypervisor 标志（部分内核会在 cpuinfo 暴露 flag）
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil && strings.Contains(string(b), " hypervisor") {
		return "virtualized"
	}
	return "bare-metal"
}

// Collect 采集一次完整快照。
func (c *Collector) Collect() model.Metrics {
	m := model.Metrics{TS: now()}

	// CPU：读两次 /proc/stat 差值由调用方间隔决定。
	// 读取与基线更新必须同锁完成，否则并发 Collect 可能让较旧的样本
	// 覆盖较新的基线，算出错误的区间使用率。
	c.cpuMu.Lock()
	if idle, tot, ok := readCPU(); ok {
		if c.prevTot > 0 && tot > c.prevTot {
			dTot := float64(tot - c.prevTot)
			dIdle := float64(idle - c.prevIdle)
			if dTot > 0 {
				m.CPUPct = clamp((dTot-dIdle)/dTot*100, 0, 100)
			}
		}
		c.prevIdle, c.prevTot = idle, tot
	}
	c.cpuMu.Unlock()

	// 负载
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		f := strings.Fields(string(b))
		if len(f) >= 4 {
			m.Load1, _ = strconv.ParseFloat(f[0], 64)
			m.Load5, _ = strconv.ParseFloat(f[1], 64)
			m.Load15, _ = strconv.ParseFloat(f[2], 64)
			if i := strings.Index(f[3], "/"); i > 0 {
				m.Procs, _ = strconv.Atoi(f[3][i+1:])
			}
		}
	}

	// 内存 / Swap（KB → 字节）
	mem := readMemInfo()
	m.MemTotal = mem["MemTotal"] * 1024
	if avail, ok := mem["MemAvailable"]; ok {
		m.MemUsed = (mem["MemTotal"] - avail) * 1024
	} else {
		m.MemUsed = (mem["MemTotal"] - mem["MemFree"] - mem["Buffers"] - mem["Cached"]) * 1024
	}
	m.SwapTotal = mem["SwapTotal"] * 1024
	m.SwapUsed = (mem["SwapTotal"] - mem["SwapFree"]) * 1024

	// 磁盘（根分区）
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err == nil {
		m.DiskTotal = st.Blocks * uint64(st.Bsize)
		m.DiskUsed = (st.Blocks - st.Bfree) * uint64(st.Bsize)
	}

	// 网络（排除 lo）
	m.NetRx, m.NetTx = readNetDev()

	// TCP 已建立连接数
	m.TCPConn = readTCPConns()

	// 运行时长
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		f := strings.Fields(string(b))
		if len(f) >= 1 {
			v, _ := strconv.ParseFloat(f[0], 64)
			m.Uptime = uint64(v)
		}
	}
	return m
}

func now() int64 { return timeNow() }

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// readCPU 返回 (idle 累计, total 累计)。
func readCPU() (uint64, uint64, bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0, 0, false
	}
	// cpu  user nice system idle iowait irq softirq steal guest guest_nice
	fields := strings.Fields(sc.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, false
	}
	var vals []uint64
	for _, s := range fields[1:] {
		v, _ := strconv.ParseUint(s, 10, 64)
		vals = append(vals, v)
	}
	var idle, total uint64
	for i, v := range vals {
		total += v
		if i == 3 || i == 4 { // idle + iowait 视为空闲
			idle += v
		}
	}
	// guest / guest_nice 已包含在 user / nice 中，直接求和会重复计算，
	// 在虚拟机上虚增 total、压低使用率。
	if len(vals) > 8 {
		total -= vals[8]
	}
	if len(vals) > 9 {
		total -= vals[9]
	}
	return idle, total, true
}

func readMemInfo() map[string]uint64 {
	out := map[string]uint64{}
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		i := strings.Index(line, ":")
		if i < 0 {
			continue
		}
		k := line[:i]
		v := strings.Fields(strings.TrimSpace(line[i+1:]))
		if len(v) > 0 {
			n, _ := strconv.ParseUint(v[0], 10, 64)
			out[k] = n
		}
	}
	return out
}

func readNetDev() (rx, tx uint64) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		i := strings.Index(line, ":")
		if i < 0 {
			continue
		}
		iface := strings.TrimSpace(line[:i])
		if skipIface(iface) {
			continue
		}
		f := strings.Fields(line[i+1:])
		if len(f) < 9 {
			continue
		}
		r, _ := strconv.ParseUint(f[0], 10, 64)
		t, _ := strconv.ParseUint(f[8], 10, 64)
		rx += r
		tx += t
	}
	return
}

// sysClassNet 是网卡拓扑信息的根目录，测试中可替换。
var sysClassNet = "/sys/class/net"

// skipIface 判断是否应跳过该网卡的计数。
//
// 只看名字不够：bond0 与其 slave（eth0/eth1）、网桥与容器 veth 会把
// 同一批报文重复累加，累计流量放大 2–3 倍，而历史层正是用这些累计值
// 做差分算流量。这里按拓扑补充两条判据：
//   - 有 master 链接 = 从设备，其流量已计入主设备；
//   - 容器/网桥侧的虚拟网卡按名字前缀跳过（无 master 但同样重复）。
func skipIface(name string) bool {
	if name == "" || name == "lo" {
		return true
	}
	for _, p := range []string{"veth", "docker", "br-", "cni", "cali", "flannel", "kube"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	if _, err := os.Lstat(filepath.Join(sysClassNet, name, "master")); err == nil {
		return true
	}
	return false
}

// readTCPConns 统计 ESTABLISHED(01) 状态的 TCP 连接数。
func readTCPConns() int {
	n := 0
	for _, p := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		first := true
		for sc.Scan() {
			if first {
				first = false
				continue
			}
			fields := strings.Fields(sc.Text())
			if len(fields) < 4 {
				continue
			}
			if fields[3] == "01" {
				n++
			}
		}
		f.Close()
	}
	return n
}

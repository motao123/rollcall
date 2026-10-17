//go:build linux

// Package collect 实现只读指标采集。
//
// ★ 安全约束（本项目的核心设计）★
//
//	· 只读 /proc 与 statfs，不读取用户数据
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
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/motao/probe/internal/model"
)

// Collector 持有上一次 CPU 采样，用于计算区间使用率。
type Collector struct {
	mu       sync.Mutex
	prevIdle uint64
	prevTot  uint64
	host     model.HostInfo
	hostOnce bool
}

func New() *Collector { return &Collector{} }

// Host 返回静态硬件信息（仅首次真正读取）。
func (c *Collector) Host() model.HostInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hostOnce {
		return c.host
	}
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
	c.host, c.hostOnce = h, true
	return h
}

func detectVirt() string {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	s := string(b)
	switch {
	case strings.Contains(s, "KVM"):
		return "KVM"
	case strings.Contains(s, "VMware"):
		return "VMware"
	case strings.Contains(s, "Microsoft"):
		return "Hyper-V"
	case strings.Contains(s, "QEMU"):
		return "QEMU"
	}
	return "bare-metal"
}

// Collect 采集一次完整快照。
func (c *Collector) Collect() model.Metrics {
	m := model.Metrics{TS: now()}

	// CPU：读两次 /proc/stat 差值由调用方间隔决定
	if idle, tot, ok := readCPU(); ok {
		c.mu.Lock()
		if c.prevTot > 0 && tot > c.prevTot {
			dTot := float64(tot - c.prevTot)
			dIdle := float64(idle - c.prevIdle)
			if dTot > 0 {
				m.CPUPct = clamp((dTot-dIdle)/dTot*100, 0, 100)
			}
		}
		c.prevIdle, c.prevTot = idle, tot
		c.mu.Unlock()
	}

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
		if iface == "lo" || strings.HasPrefix(iface, "veth") || strings.HasPrefix(iface, "docker") {
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

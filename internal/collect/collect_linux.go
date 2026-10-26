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
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/motao123/rollcall/internal/model"
)

// Collector 持有上一次 CPU 采样，用于计算区间使用率。
type Collector struct {
	cpuMu      sync.Mutex
	prevIdle   uint64
	prevTot    uint64
	prevSteal  uint64
	prevIowait uint64
	hostOnce   sync.Once
	host       model.HostInfo
}

func New() *Collector { return &Collector{} }

// Supported 表示当前平台有真实采集实现。
func Supported() bool { return true }

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
		h.CPUModel = readCPUModel()
		// 虚拟化类型：KVM / VMware / 物理机
		h.Virt = detectVirt()
		c.host = h
	})
	return c.host
}

// cpuModelKeys 是各架构 /proc/cpuinfo 里表示型号的键。
// x86 用 "model name"，arm64 用 "Hardware" 或 "model name"，
// ppc64 用 "cpu"。注意 ARM64/ppc64 的 "processor" 行数值是逻辑核编号，
// 只有 x86 的 "processor" 才带型号描述，因此把它放在最后并跳过纯数字。
var cpuModelKeys = []string{"model name", "hardware", "cpu model", "cpu", "processor"}

// readCPUModel 提取 CPU 型号。
// 只认 "model name" 时，arm64 机器的型号永远是空字符串（面板上不可见）。
// procCPUInfo 是 cpuinfo 路径，测试中可替换。
var procCPUInfo = "/proc/cpuinfo"

func readCPUModel() string {
	b, err := os.ReadFile(procCPUInfo)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(b), "\n")
	for _, key := range cpuModelKeys {
		for _, line := range lines {
			i := strings.Index(line, ":")
			if i <= 0 {
				continue
			}
			if !strings.EqualFold(strings.TrimSpace(line[:i]), key) {
				continue
			}
			v := strings.TrimSpace(line[i+1:])
			// ARM64/ppc64: "processor : 0" 是核编号，不是型号。
			if key == "processor" && isAllDigits(v) {
				continue
			}
			if v != "" {
				return v
			}
		}
	}
	return ""
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// detectVirt 判断虚拟化类型。
//
// 不能只看 /proc/cpuinfo 的 CPU 型号：KVM 直通/宿主机型号透传时
// cpuinfo 里通常不含 "KVM"（hypervisor vendor 在 CPUID 0x40000000，
// /proc/cpuinfo 不暴露），Hyper-V 也很少带 "Microsoft"。
// 按 systemd-detect-virt 的思路优先读 /sys 与 DMI。
func detectVirt() string {
	if b, err := os.ReadFile("/sys/hypervisor/type"); err == nil {
		// 该文件给的是小写标识（xen / kvm / vmware …）。
		// 原样首字母大写会产出 "Kvm"、"Hyperv" 这类半成品，
		// 与 DMI 分支的 "KVM"/"Hyper-V" 不一致。
		switch v := strings.ToLower(strings.TrimSpace(string(b))); v {
		case "xen":
			return "Xen"
		case "kvm":
			return "KVM"
		case "vmware":
			return "VMware"
		case "hyperv", "microsoft":
			return "Hyper-V"
		case "qemu":
			return "QEMU"
		case "bochs":
			return "Bochs"
		case "parallels":
			return "Parallels"
		case "bhyve":
			return "bhyve"
		case "":
			// 落到 DMI 分支
		default:
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
		case strings.Contains(s, "hyper-v"), strings.Contains(s, "virtual machine"):
			// 只认明确的虚拟化标识。不能单看 "microsoft"：Surface 等
			// 微软裸机的 product_name 里也带这个词，会被误判成 Hyper-V 虚拟机。
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
	if idle, tot, steal, iowait, ok := readCPU(); ok {
		if c.prevTot > 0 && tot > c.prevTot {
			dTot := float64(tot - c.prevTot)
			// idle 含 iowait，内核允许它回退：回退时这一轮按 0 计，
			// 宁可少报一点 CPU，也不要算出一个负数再下溢成天文数字。
			dIdle := float64(0)
			if idle >= c.prevIdle {
				dIdle = float64(idle - c.prevIdle)
			}
			if dTot > 0 {
				m.CPUPct = clamp((dTot-dIdle)/dTot*100, 0, 100)
				// steal 是被宿主机拿走的时间片：VPS 上高 steal 意味着邻居超售，
				// 这是「机器变卡但自己进程不忙」的直接证据。
				//
				// ★ 三个计数器各自比自己上一轮的值。原来这里写成 `tot > c.prevSteal`
				// （拿累计总 jiffies 比上一轮的 steal），恒为真等于没防护：
				// 内核允许 iowait 回退（idle 里含 iowait，会跟着回退），vCPU 迁移/热插拔
				// 也会让 steal 回退。回退时无符号相减下溢成 1.8e19，
				// 结果 CPU 曲线瞬间归零、iowait/steal 被 clamp 成 100，还喂给告警。
				if steal >= c.prevSteal {
					m.StealPct = clamp(float64(steal-c.prevSteal)/dTot*100, 0, 100)
				}
				if iowait >= c.prevIowait {
					m.IowaitPct = clamp(float64(iowait-c.prevIowait)/dTot*100, 0, 100)
				}
			}
		}
		c.prevIdle, c.prevTot = idle, tot
		c.prevSteal, c.prevIowait = steal, iowait
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
	// /proc/meminfo 缺键时 readMemInfo 返回 0：所有减法都必须先比较，
	// 否则 uint64 回绕成约 1.8e19 字节，面板与历史会记录天文数字。
	mem := readMemInfo()
	m.MemTotal = mem["MemTotal"] * 1024
	if avail, ok := mem["MemAvailable"]; ok {
		if mem["MemTotal"] > avail {
			m.MemUsed = (mem["MemTotal"] - avail) * 1024
		}
	} else {
		buffered := mem["MemFree"] + mem["Buffers"] + mem["Cached"]
		if mem["MemTotal"] > buffered {
			m.MemUsed = (mem["MemTotal"] - buffered) * 1024
		}
	}
	if m.MemUsed > m.MemTotal {
		m.MemUsed = m.MemTotal
	}
	m.SwapTotal = mem["SwapTotal"] * 1024
	if mem["SwapTotal"] > mem["SwapFree"] {
		m.SwapUsed = (mem["SwapTotal"] - mem["SwapFree"]) * 1024
	}

	// 磁盘（根分区）
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err == nil {
		m.DiskTotal = st.Blocks * uint64(st.Bsize)
		m.DiskUsed = (st.Blocks - st.Bfree) * uint64(st.Bsize)
	}

	// 网络（排除 lo）：总量用于流量配额，明细用于面板展示
	m.NetRx, m.NetTx, m.NICs = readNetDev()

	// 温度（hwmon 传感器的最高值）。多数 VPS 没有传感器，这里就是 0，
	// 字段整个缺席 —— 对"没有数据"与"真的是 0℃"必须可区分。
	m.TempC = readTempC()

	// 磁盘 IO 累计扇区数与多挂载点
	m.DiskReadBytes, m.DiskWriteBytes = readDiskIO()
	m.Mounts = readMounts()

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

// readCPU 返回 (idle 累计, total 累计, steal 累计, iowait 累计)。
// steal/iowait 在旧内核/某些容器里可能缺列，此时返回 0，上层按"无变化"跳过。
func readCPU() (uint64, uint64, uint64, uint64, bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, 0, 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0, 0, 0, 0, false
	}
	// cpu  user nice system idle iowait irq softirq steal guest guest_nice
	fields := strings.Fields(sc.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, 0, 0, false
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
	var steal, iowait uint64
	if len(vals) > 7 {
		steal = vals[7]
	}
	if len(vals) > 4 {
		iowait = vals[4]
	}
	return idle, total, steal, iowait, true
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

func readNetDev() (rx, tx uint64, nics []model.NICInfo) {
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
		nics = append(nics, model.NICInfo{Name: iface, Rx: r, Tx: t})
	}
	sort.Slice(nics, func(i, j int) bool { return nics[i].Rx+nics[i].Tx > nics[j].Rx+nics[j].Tx })
	return
}

// readTempC 返回 hwmon 传感器的最高温度（℃）；找不到任何有效读数时返回 0。
//
// 只认 >0 的读数：部分虚拟化环境会暴露恒为 0 的传感器文件，
// 那种"0℃"不是数据，混进来会把真实温度永远压成 0。
// 取最大值而不是平均值：温度告警关心的是"最热的那一点"。
func readTempC() float64 {
	entries, err := os.ReadDir("/sys/class/hwmon")
	if err != nil {
		return 0
	}
	var maxC float64
	for _, e := range entries {
		inputs, _ := filepath.Glob("/sys/class/hwmon/" + e.Name() + "/temp*_input")
		for _, p := range inputs {
			b, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			milli, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
			if err != nil {
				continue
			}
			if c := float64(milli) / 1000; c > maxC {
				maxC = c
			}
		}
	}
	return maxC
}

// readDiskIO 汇总整盘读写扇区（diskstats 固定以 512 字节为单位）。
// 跳过分区、dm/md/loop/ram：同时统计整盘与分区、或物理盘与 dm 映射
// 会把同一份 IO 重复累加。
func readDiskIO() (read, write uint64) {
	b, err := os.ReadFile("/proc/diskstats")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 10 || !wholeDisk(f[2]) {
			continue
		}
		r, _ := strconv.ParseUint(f[5], 10, 64)
		w, _ := strconv.ParseUint(f[9], 10, 64)
		read += r * 512
		write += w * 512
	}
	return
}

func wholeDisk(name string) bool {
	for _, p := range []string{"loop", "ram", "dm-", "md", "sr", "fd", "zram", "nbd"} {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	if strings.HasPrefix(name, "nvme") {
		// nvme0n1 是整盘，nvme0n1p1 是分区。
		return !strings.Contains(name, "p")
	}
	if strings.HasPrefix(name, "mmcblk") {
		return !strings.Contains(name, "p")
	}
	for _, p := range []string{"sd", "vd", "xvd", "hd"} {
		if strings.HasPrefix(name, p) {
			rest := name[len(p):]
			if rest == "" {
				return false
			}
			for _, c := range rest {
				if c < 'a' || c > 'z' {
					return false
				}
			}
			return true
		}
	}
	return false
}

// readMounts 采集真实文件系统的挂载点用量。
// 伪文件系统与容器运行时目录没有容量意义，统一跳过；按设备去重，
// 避免 bind mount 把同一分区重复计入。
func readMounts() []model.MountInfo {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil
	}
	defer f.Close()
	skipFS := map[string]bool{
		"proc": true, "sysfs": true, "devpts": true, "tmpfs": true, "devtmpfs": true,
		"cgroup": true, "cgroup2": true, "securityfs": true, "pstore": true,
		"bpf": true, "debugfs": true, "tracefs": true, "configfs": true,
		"fusectl": true, "mqueue": true, "hugetlbfs": true, "autofs": true,
		"rpc_pipefs": true, "nsfs": true, "binfmt_misc": true, "squashfs": true,
		"fuse.gvfsd-fuse": true, "fuse.portal": true,
		// ★ 网络/分布式文件系统一律跳过容量采集。
		// 原因不是"不需要"，而是 statfs(2) 不可取消：NFS/CIFS 服务器失联时
		// 它会阻塞在 D 状态，Go 侧没有任何办法超时或中断，采集协程就此挂死
		// （后续所有采集都停摆，且没有报错）。这是已知的内核限制，
		// 唯一安全的做法是不对这类挂载点发起 statfs。
		// 这类存储的容量请用独立探针或服务商控制台查看。
		"nfs": true, "nfs4": true, "cifs": true, "smb3": true, "smbfs": true,
		"sshfs": true, "glusterfs": true, "ceph": true, "lustre": true,
		"9p": true, "afs": true, "davfs": true, "ncpfs": true, "fuse.sshfs": true,
		"fuse.rclone": true, "fuse.s3fs": true, "fuse.glusterfs": true,
	}
	seen := map[string]bool{}
	var out []model.MountInfo
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		dev := unescapeMount(fields[0])
		path := unescapeMount(fields[1])
		fstype := fields[2]
		if skipFS[fstype] || strings.HasPrefix(fstype, "fuse.") {
			continue
		}
		if dev == "none" || strings.HasPrefix(path, "/snap") ||
			strings.HasPrefix(path, "/proc") || strings.HasPrefix(path, "/sys") ||
			strings.HasPrefix(path, "/dev") || isRuntimePath(path) ||
			strings.HasPrefix(path, "/var/lib/docker") {
			continue
		}
		// 同一设备只取第一个挂载点。★ 必须在 statfs **成功之后**才标记 seen：
		// 否则首个挂载点因为权限/驱动原因 statfs 失败时，同一设备后面那个
		// 能正常取到的挂载点也会被跳过，磁盘容量直接缺失。
		if seen[dev] {
			continue
		}
		var st syscall.Statfs_t
		if err := syscall.Statfs(path, &st); err != nil || st.Blocks == 0 {
			continue
		}
		seen[dev] = true
		total := st.Blocks * uint64(st.Bsize)
		used := (st.Blocks - st.Bfree) * uint64(st.Bsize)
		out = append(out, model.MountInfo{Path: path, Used: used, Total: total})
		if len(out) >= 12 {
			break
		}
	}
	// 根分区放最前，其余按路径排序，保证面板顺序稳定。
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path == "/" || out[j].Path == "/" {
			return out[i].Path == "/" && out[j].Path != "/"
		}
		return out[i].Path < out[j].Path
	})
	return out
}

func unescapeMount(s string) string {
	r := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return r.Replace(s)
}

// sysClassNet 是网卡拓扑信息的根目录，测试中可替换。
var sysClassNet = "/sys/class/net"

// skipIface 判断是否应跳过该网卡的计数。
//
// 只看名字不够：bond0 与其 slave（eth0/eth1）、网桥与容器 veth 会把
// 同一批报文重复累加，累计流量放大 2–3 倍，而历史层正是用这些累计值
// 做差分算流量。这里按拓扑补充两条判据：
//   - 有 master 链接 = 从设备，其流量已计入主设备；
//   - 容器/网桥侧的虚拟网卡按名字前缀跳过（无 master 但同样重复）；
//   - macvlan/ipvlan/vlan 没有 master，但有 lower_* 链接指向承载网卡，
//     报文同时计入 lower 设备，必须跳过以免流量翻倍。
//
// isRuntimePath 判断是否为内核/运行时专用挂载点。
//
// 不能简单用 /run 前缀：/run/media/<user>/<disk> 是 udisks2 挂载的真实
// 可移动介质，被一起跳过就看不到这些分区。
func isRuntimePath(path string) bool {
	for _, p := range []string{
		"/run/systemd", "/run/snapd", "/run/udev", "/run/lock",
		"/run/credentials", "/run/user", "/run/dbus", "/run/NetworkManager",
	} {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

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
	if lower, _ := filepath.Glob(filepath.Join(sysClassNet, name, "lower_*")); len(lower) > 0 {
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

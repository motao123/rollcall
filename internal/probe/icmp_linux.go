//go:build linux

package probe

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"
)

// pingOnce 发一个 ICMP echo request 并等回复，返回往返时间。
//
// 优先用**非特权**路径（SOCK_DGRAM + IPPROTO_ICMP）：只要
// net.ipv4.ping_group_range 覆盖当前进程的组，普通用户就能收发 echo，
// 既不需要 root 也不需要 CAP_NET_RAW。失败时才退回 SOCK_RAW ——
// 这条路需要 CAP_NET_RAW，是给"内核关了 unprivileged ping"的老系统留的。
//
// 两条路的收包形态不同（raw 带 IP 头、dgram 不带），由 echoReply 统一处理。
func pingOnce(ip net.IP, timeout time.Duration) (time.Duration, error) {
	v4 := ip.To4()
	if v4 == nil {
		return 0, errICMPv6
	}

	fd, err := openICMPv4()
	if err != nil {
		return 0, err
	}
	defer syscall.Close(fd)

	// 每次等待用 SO_RCVTIMEO 兜住；整体截止时间另算，避免收到无关包后
	// 重新计时导致总时长变成"包数 × timeout"。
	tv := syscall.NsecToTimeval(timeout.Nanoseconds())
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv); err != nil {
		return 0, fmt.Errorf("设置接收超时失败: %w", err)
	}

	seq := uint16(time.Now().UnixNano() & 0xffff)
	payload := make([]byte, icmpPayload)
	copy(payload, "rollcall-ping")
	binary.BigEndian.PutUint16(payload[8:10], seq)

	pkt := buildEcho(0, seq, payload)
	sa := &syscall.SockaddrInet4{}
	copy(sa.Addr[:], v4)

	start := time.Now()
	if err := syscall.Sendto(fd, pkt, 0, sa); err != nil {
		return 0, fmt.Errorf("发送 echo 失败: %w", err)
	}

	deadline := start.Add(timeout)
	buf := make([]byte, 1024)
	for time.Now().Before(deadline) {
		n, _, rerr := syscall.Recvfrom(fd, buf, 0)
		if rerr != nil {
			if errors.Is(rerr, syscall.EAGAIN) || errors.Is(rerr, syscall.EWOULDBLOCK) ||
				errors.Is(rerr, syscall.EINTR) {
				// 超时或被打断：交给外层循环按 deadline 判断是否还等
				continue
			}
			return 0, fmt.Errorf("接收 echo 回复失败: %w", rerr)
		}
		msg, ok := echoReply(buf, n)
		if !ok {
			continue
		}
		if msg[0] != icmpEchoReply || msg[1] != 0 {
			continue
		}
		// id 在 dgram socket 上由内核改写，只能靠 seq 匹配。
		// 本函数每次调用都新建 socket，所以窗口内不存在同 seq 的干扰包。
		if binary.BigEndian.Uint16(msg[6:8]) != seq {
			continue
		}
		return time.Since(start), nil
	}
	return 0, errors.New("等待 echo 回复超时（目标可能屏蔽了 ICMP）")
}

// openICMPv4 打开一个可以收发 ICMP echo 的 socket。
func openICMPv4() (int, error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, syscall.IPPROTO_ICMP)
	if err == nil {
		syscall.CloseOnExec(fd)
		return fd, nil
	}
	dgramErr := err

	fd, err = syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_ICMP)
	if err == nil {
		syscall.CloseOnExec(fd)
		return fd, nil
	}
	// 两条路都失败时才报错，并把"该怎么修"写清楚：这条信息会显示在
	// 面板的服务详情里，运维第一眼就能知道是权限问题而不是网络问题。
	if errors.Is(dgramErr, syscall.EPERM) || errors.Is(dgramErr, syscall.EACCES) ||
		errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		return -1, fmt.Errorf("没有 ICMP 权限：请确认 sysctl net.ipv4.ping_group_range "+
			"覆盖服务用户的组（当前 dgram 失败: %v；raw 失败: %v）", dgramErr, err)
	}
	return -1, fmt.Errorf("创建 ICMP socket 失败: dgram=%v raw=%v", dgramErr, err)
}

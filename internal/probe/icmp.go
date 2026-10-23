package probe

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/motao123/rollcall/internal/model"
)

// ICMP 探测（ping）。
//
// 与 http/tcp 的区别在于"它回答的是另一个问题"：http/tcp 问"这个服务活没活"，
// icmp 问"这台机器在不在线、往返要多久"。服务全挂但机器还在的情境里，
// 只有 icmp 能区分"网络断了"和"进程挂了"。
//
// 收发走**非特权 datagram socket**（SOCK_DGRAM + IPPROTO_ICMP），
// 只要内核允许（net.ipv4.ping_group_range 覆盖进程的组）就不需要 root
// 或 CAP_NET_RAW。这也是不给服务端加 capability 的原因：能用非特权路径
// 解决的事，不要用提权解决。
//
// 限制：目前只做 IPv4。ICMPv6 的校验和计算由内核在 raw socket 上代劳的
// 行为与 IPv4 不同，两条路径的差异没有实机验证过的东西不写进来 ——
// 遇到 IPv6 目标会明确报错，而不是假装探测过。

const (
	icmpEchoRequest = 8
	icmpEchoReply   = 0
	// icmpPayload 是echo 载荷长度。只用来自证"这个回复是我发的"，
	// 16 字节足够且不会让包超过常见 MTU。
	icmpPayload = 16
	// icmpTimeout 是单次等待回包的上限。选 2 秒：跨洲 RTT 通常 < 400ms，
	// 2 秒既能容忍一次重传，又不会让一轮探测长时间挂住。
	icmpTimeout = 2 * time.Second
)

var errICMPv6 = errors.New("ICMP 探测目前只支持 IPv4 目标")

// parseICMPTarget 解析 ICMP 目标：主机名或 IP 字面量，不带协议与端口。
func parseICMPTarget(target string) (string, error) {
	s := strings.TrimSpace(target)
	if s == "" {
		return "", errors.New("ICMP 目标不能为空")
	}
	if strings.Contains(s, "://") {
		return "", errors.New("ICMP 目标不要带协议前缀，只填主机名或 IP")
	}
	if strings.ContainsAny(s, "/ \t") {
		return "", errors.New("ICMP 目标不能包含路径或空白")
	}
	s = strings.TrimSuffix(s, ".")
	// 顺序很重要：先认 IP 字面量，再看冒号是不是"误填端口"。
	// 裸 IPv6（2606:4700::1111）本身含冒号，若先判冒号就会把它当成带端口的
	// 主机名拒掉 —— 生产验证时正是这么踩到的。
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		s = s[1 : len(s)-1]
	}
	if !isIPLiteral(s) && strings.Contains(s, ":") {
		return "", errors.New("ICMP 目标不要带端口，只填主机名或 IP")
	}
	if len(s) == 0 || len(s) > 253 {
		return "", errors.New("ICMP 目标长度不合法")
	}
	if isIPLiteral(s) {
		return s, nil
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 {
			return "", errors.New("ICMP 目标不是合法的主机名或 IP")
		}
		for _, c := range label {
			ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
				(c >= '0' && c <= '9') || c == '-' || c == '_'
			if !ok {
				return "", errors.New("ICMP 目标包含非法字符")
			}
		}
	}
	return s, nil
}

// isIPLiteral 判断一个目标是不是 IP 字面量。
//
// 必须用 net.ParseIP 而不是"含冒号就算 IPv6"这种形状判断：后者会把
// example.com:80 当成 IPv6 放行，于是"误填端口"这个最常见的输入错误
// 一路走到探测阶段才失败。地址是否可用仍由 netguard.CheckIP 判定。
func isIPLiteral(s string) bool {
	return net.ParseIP(s) != nil
}

// icmpChecksum 计算 RFC 1071 的 16 位反码和。
func icmpChecksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

// buildEcho 构造一个 ICMP echo request。
//
// id 在非特权 datagram socket 上会被内核改写成本 socket 的标识，
// 因此匹配回复只用 seq；id 传 0 即可。
func buildEcho(id, seq uint16, payload []byte) []byte {
	pkt := make([]byte, 8+len(payload))
	pkt[0] = icmpEchoRequest
	pkt[1] = 0
	binary.BigEndian.PutUint16(pkt[4:6], id)
	binary.BigEndian.PutUint16(pkt[6:8], seq)
	copy(pkt[8:], payload)
	binary.BigEndian.PutUint16(pkt[2:4], icmpChecksum(pkt))
	return pkt
}

// echoReply 从收到的原始字节里找出 ICMP 消息体。
//
// 非特权 socket 收到的数据从 ICMP 头开始；raw socket 则带 IP 头。
// 两种都可能出现（取决于内核与权限），按版本号前缀区分而不是"试两次"。
func echoReply(buf []byte, n int) (msg []byte, ok bool) {
	if n < 8 {
		return nil, false
	}
	buf = buf[:n]
	if buf[0]>>4 == 4 {
		ihl := int(buf[0]&0x0f) * 4
		if ihl < 20 || n < ihl+8 {
			return nil, false
		}
		buf = buf[ihl:]
	} else if buf[0]>>4 == 6 {
		return nil, false // IPv6 头：不属于本路径
	}
	return buf, true
}

// doICMP 执行一次 ping 并填写结果。
func (r *Runner) doICMP(sv model.Service, res *model.ServiceResult) {
	host, err := parseICMPTarget(sv.Target)
	if err != nil {
		res.Detail = err.Error()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ips, err := r.policy.ResolveAllowed(ctx, host)
	if err != nil {
		res.Detail = "blocked: " + trimErr(err)
		return
	}
	// 依次试解析出来的地址：双栈目标里 v4 可能只是其中一条记录，
	// 而本实现只做 v4，遇到 v6 先跳过而不是直接判失败。
	var lastErr error
	tried := 0
	for _, ip := range ips {
		if ip.To4() == nil {
			lastErr = errICMPv6
			continue
		}
		tried++
		rtt, perr := pingOnce(ip, icmpTimeout)
		if perr == nil {
			res.OK = true
			res.LatencyMs = int(rtt.Milliseconds())
			if res.LatencyMs == 0 {
				// 亚毫秒的 RTT 不能写成 0：0 在图表与"未测到"同形，
				// 而这里明确是"测到了，很快"。
				res.LatencyMs = 1
			}
			res.Detail = "echo reply from " + ip.String()
			return
		}
		lastErr = perr
	}
	if tried == 0 && lastErr == nil {
		lastErr = errors.New("没有可用的 IPv4 地址")
	}
	if lastErr != nil {
		res.Detail = trimErr(lastErr)
	}
}

package probe

import (
	"encoding/binary"
	"net"
	"strings"
	"testing"

	"github.com/motao123/rollcall/internal/model"
)

// TestParseICMPTarget 覆盖目标格式的接受与拒绝。
//
// 这些错误信息会直接显示在面板上，所以断言的是"用户看得懂的那句话"，
// 而不只是"报错了"。
func TestParseICMPTarget(t *testing.T) {
	ok := []string{"example.com", "1.1.1.1", "vps-01.example.com", "  example.com  ",
		"example.com.", "[2606:4700::1111]"}
	for _, in := range ok {
		if _, err := parseICMPTarget(in); err != nil {
			t.Fatalf("%q 应被接受，实际 %v", in, err)
		}
	}

	bad := map[string]string{
		"":                       "不能为空",
		"http://example.com":     "协议",
		"example.com/path":       "路径",
		"example.com:80":         "端口",
		"1.1.1.1:22":             "端口",
		"exa mple.com":           "空白",
		"例子.中国":                  "非法字符",
		strings.Repeat("a", 260): "长度",
	}
	for in, want := range bad {
		_, err := parseICMPTarget(in)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%q 期望错误含 %q，实际 %v", in, want, err)
		}
	}
}

// TestICMPChecksum 用 RFC 1071 的性质验证：把校验和填回去再求和，结果应为 0xffff。
func TestICMPChecksum(t *testing.T) {
	pkt := buildEcho(0, 7, []byte("rollcall-ping\x00\x07"))
	if pkt[0] != icmpEchoRequest || pkt[1] != 0 {
		t.Fatalf("echo 头不对: type=%d code=%d", pkt[0], pkt[1])
	}
	if got := binary.BigEndian.Uint16(pkt[6:8]); got != 7 {
		t.Fatalf("seq 应为 7，实际 %d", got)
	}
	// 奇数长度也要覆盖：补零逻辑容易写错。
	odd := buildEcho(1, 2, []byte("abc"))
	for _, b := range [][]byte{pkt, odd} {
		if sum := icmpChecksum(b); sum != 0 {
			t.Fatalf("含校验和的包再求和应为 0，实际 %#x", sum)
		}
	}
}

// TestEchoReplyStripsIPHeader 覆盖两种收包形态：路径是否认清 IP 头。
func TestEchoReplyStripsIPHeader(t *testing.T) {
	icmpMsg := buildEcho(0, 9, []byte("abcdefgh12345678"))

	// 1) 非特权 datagram socket：直接从 ICMP 头开始，长度与内容原样
	msg, ok := echoReply(icmpMsg, len(icmpMsg))
	if !ok {
		t.Fatal("无 IP 头的包应被接受")
	}
	if len(msg) != len(icmpMsg) || msg[0] != icmpEchoRequest {
		t.Fatalf("无 IP 头时不应改动内容: len=%d type=%d", len(msg), msg[0])
	}

	// 2) raw socket：前面是 20 字节 IP 头（0x45）
	raw := make([]byte, 20+len(icmpMsg))
	raw[0] = 0x45
	raw[2] = 0
	raw[3] = byte(20 + len(icmpMsg))
	copy(raw[20:], icmpMsg)
	msg, ok = echoReply(raw, len(raw))
	if !ok {
		t.Fatal("带 IP 头的包应被剥离后接受")
	}
	if len(msg) != len(icmpMsg) || msg[6] != icmpMsg[6] {
		t.Fatalf("剥离结果不对: len=%d", len(msg))
	}

	// 3) IPv6 头（0x60）不属于本路径，必须拒绝而不是按 ICMP 解析
	v6 := make([]byte, 48)
	v6[0] = 0x60
	if _, ok := echoReply(v6, len(v6)); ok {
		t.Fatal("IPv6 头不应被当成 ICMPv4 处理")
	}

	// 4) 截断的包
	if _, ok := echoReply(icmpMsg[:5], 5); ok {
		t.Fatal("不足 8 字节应拒绝")
	}
}

// TestValidateServiceICMP 确认 icmp 类型能入库，且沿用同一套 SSRF 限制。
func TestValidateServiceICMP(t *testing.T) {
	r := NewRunner(Config{})
	if err := r.ValidateService(model.Service{Type: "icmp", Target: "example.com"}); err != nil {
		t.Fatalf("icmp 目标应合法: %v", err)
	}
	if err := r.ValidateService(model.Service{Type: "icmp", Target: "example.com:80"}); err == nil {
		t.Fatal("带端口应被拒绝")
	}
	// 私网地址默认拒绝：与 http/tcp 一致，换类型不能绕过策略
	if err := r.ValidateService(model.Service{Type: "icmp", Target: "10.0.0.1"}); err == nil {
		t.Fatal("私网地址默认应被拒绝")
	}
	// 开了 AllowPrivate 才放行
	r2 := NewRunner(Config{AllowPrivate: true})
	if err := r2.ValidateService(model.Service{Type: "icmp", Target: "10.0.0.1"}); err != nil {
		t.Fatalf("AllowPrivate 下应放行: %v", err)
	}
}

// TestDoICMPLoopback 真发一个 echo（仅 Linux 且内核允许非特权 ping 时有效）。
// 环境不支持时跳过并说明原因：这是能力探测，不是功能断言。
func TestDoICMPLoopback(t *testing.T) {
	if _, err := pingOnce(net.IPv4(127, 0, 0, 1), icmpTimeout); err != nil {
		t.Skipf("当前环境不支持 ICMP（%v），跳过", err)
	}
	r := NewRunner(Config{AllowPrivate: true})
	res := r.Do(model.Service{ID: "s1", Type: "icmp", Target: "127.0.0.1"})
	if !res.OK {
		t.Fatalf("回环 ping 应成功，实际 %+v", res)
	}
	if res.LatencyMs < 0 {
		t.Fatalf("延迟不应为负: %d", res.LatencyMs)
	}
}

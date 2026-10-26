package main

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// sniffListener 区分"TLS 握手"与"明文 HTTP"。
//
// 背景：Go 的 ServeTLS 收到明文请求时只会回一行
//
//	400 Bad Request / Client sent an HTTP request to an HTTPS server.
//
// 在浏览器里这就是**一片空白** —— 用户看到的是"页面什么都没有"，
// 而不是"你用错了协议"。加了 HTTPS 之后，旧的 http:// 书签、
// 手输地址、以及别的系统里写死的地址都会撞上这个 400。
//
// 这里把明文请求改成 301 跳到同端口的 https：既不影响 TLS（TLS 记录
// 首字节固定是 0x16），也不像"两边都监听"那样多开一个端口。
type sniffListener struct {
	net.Listener
	// onPlaintext 用来记录（限流后的）提示日志，可为 nil。
	onPlaintext func(host, path string)
}

const tlsHandshakeByte = 0x16 // TLS record type: handshake

func (l *sniffListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		br := bufio.NewReader(c)
		// 只等 5 秒：连上却不发数据的连接不能把 Accept 卡死。
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		b, perr := br.Peek(1)
		_ = c.SetReadDeadline(time.Time{})
		if perr != nil || len(b) == 0 {
			c.Close()
			continue
		}
		if b[0] == tlsHandshakeByte {
			// 是 TLS：把已缓冲的字节一起交回去，不能丢。
			return &prefixConn{Conn: c, r: br}, nil
		}
		// 是明文：单独处理并继续接受下一个连接（不把它交给 http.Server，
		// 否则会出现"明文请求被 TLS 栈拒绝"的那行 400）。
		go answerHTTPSRedirect(c, br, l.onPlaintext)
	}
}

// prefixConn 让上层从 bufio.Reader 读（已窥探过的字节不会丢），
// 写与关闭仍然走原始连接。
type prefixConn struct {
	net.Conn
	r *bufio.Reader
}

func (p *prefixConn) Read(b []byte) (int, error) { return p.r.Read(b) }

// answerHTTPSRedirect 读一行请求行，回 301 指向同端口的 https。
func answerHTTPSRedirect(c net.Conn, br *bufio.Reader, onPlaintext func(string, string)) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))

	// 只读第一行与 Host 头：足够构造跳转，也不会因为大请求体卡住。
	var reqLine, host string
	for i := 0; i < 30; i++ {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if i == 0 {
			reqLine = line
		} else if line == "" {
			break
		} else if strings.HasPrefix(strings.ToLower(line), "host:") {
			host = strings.TrimSpace(line[5:])
		}
	}

	target, ok := httpsURL(reqLine, host)
	if onPlaintext != nil && ok {
		onPlaintext(host, target)
	}
	if !ok {
		// 不是可识别的 HTTP 请求：回一个最小的 400，不猜测。
		_, _ = c.Write([]byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
		return
	}
	body := fmt.Sprintf("改用 https 访问：%s\n", target)
	resp := fmt.Sprintf("HTTP/1.1 301 Moved Permanently\r\n"+
		"Location: %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\n"+
		"Connection: close\r\n\r\n%s", target, len(body), body)
	_, _ = c.Write([]byte(resp))
}

// httpsURL 从请求行与 Host 拼出 https 地址。
// 第二个返回值为 false 表示这不是一个能安全跳转的请求（例如 Host 为空）。
func httpsURL(reqLine, host string) (string, bool) {
	parts := strings.Fields(reqLine)
	if len(parts) < 2 {
		return "", false
	}
	method, path := parts[0], parts[1]
	// 只对 GET/HEAD 做跳转：其他方法（POST 表单、API 写操作）跳过去
	// 会丢掉请求体，宁可让客户端看到 400 也不要制造一个"看似成功"的空请求。
	if method != http.MethodGet && method != http.MethodHead {
		return "", false
	}
	if !strings.HasPrefix(path, "/") {
		// 绝对形式的请求行（http://host/path）只取路径部分
		if i := strings.Index(path, "://"); i >= 0 {
			rest := path[i+3:]
			if j := strings.IndexByte(rest, '/'); j >= 0 {
				path = rest[j:]
			} else {
				path = "/"
			}
		}
	}
	if strings.TrimSpace(host) == "" {
		return "", false
	}
	// Host 可能自带 :80 端口 —— 换成 https 时不能沿用 80。
	h := strings.TrimSpace(host)
	if strings.HasSuffix(h, ":80") {
		h = strings.TrimSuffix(h, ":80")
	}
	return "https://" + h + path, true
}

// logPlaintextOnce 让"有人用明文访问"这件事最多每分钟打扰一次日志。
type plaintextLogger struct {
	mu   sync.Mutex
	last int64
}

func (p *plaintextLogger) log(host, target string) {
	now := time.Now().Unix()
	p.mu.Lock()
	skip := now-p.last < 60
	if !skip {
		p.last = now
	}
	p.mu.Unlock()
	if skip {
		return
	}
	log.Printf("收到明文 HTTP 请求，已跳转到 %s（Host=%s）；若持续出现，说明有地方仍写着 http://", target, host)
}

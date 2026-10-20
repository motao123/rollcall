// Package probe 实现服务可用性探测。
//
// 探测由**服务端发起**（而非 Agent），因此即使某台 Agent 被攻破，
// 也无法伪造其他节点的服务状态。
package probe

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/netguard"
)

// Config 控制探测的出站行为。
type Config struct {
	// AllowPrivate 允许探测回环/私网目标（默认拒绝，防止 SSRF）。
	AllowPrivate bool
	// InsecureTLS 跳过 HTTPS 证书链校验（仅用于自签名目标）。
	// 默认开启校验：证书过期、自签名或域名不匹配都会导致探测失败。
	// 跳过校验时 CertDays 仍取自叶子证书，仅作展示，不代表链可信。
	InsecureTLS bool
}

// Runner 负责按间隔执行探测。
type Runner struct {
	client *http.Client
	policy netguard.Policy
}

func NewRunner(cfg Config) *Runner {
	policy := netguard.Policy{AllowPrivate: cfg.AllowPrivate}
	transport := &http.Transport{
		DialContext:           policy.DialContext,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: cfg.InsecureTLS},
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: 8 * time.Second,
	}
	r := &Runner{policy: policy}
	r.client = &http.Client{
		Timeout:   10 * time.Second,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("重定向次数过多")
			}
			// 每一跳都重新校验，阻止 3xx 跳转到内网或非 HTTP 协议
			return r.policy.CheckURL(req.URL)
		},
	}
	return r
}

// ValidateService 在入库前校验探测目标，返回用户可读的错误。
func (r *Runner) ValidateService(sv model.Service) error {
	typ := strings.ToLower(sv.Type)
	switch typ {
	case "http", "https":
		u, err := url.Parse(strings.TrimSpace(sv.Target))
		if err != nil || u.Host == "" {
			return errors.New("HTTP 目标需为完整 URL，例如 https://example.com/health")
		}
		// 类型 https 必须配 https 目标：否则 CertDays 永远是 -1，
		// 证书到期监控会静默失效。反过来 type=http 允许探测 https 站点
		// （只是把 http/https 都当作 HTTP 探测，证书天数仍会采集）。
		if typ == "https" && u.Scheme != "https" {
			return errors.New("类型 https 的目标必须使用 https://")
		}
		if u.User != nil {
			return errors.New("HTTP 目标不能包含用户名/密码")
		}
		if p := u.Port(); p != "" {
			n, err := strconv.Atoi(p)
			if err != nil || n < 1 || n > 65535 {
				return errors.New("HTTP 端口必须在 1–65535 之间")
			}
		}
		if err := r.policy.CheckURL(u); err != nil {
			return err
		}
		return nil
	case "tcp":
		addr, err := parseTCPTarget(sv.Target)
		if err != nil {
			return err
		}
		host, _, _ := net.SplitHostPort(addr)
		return r.policy.CheckHost(host)
	case "dns":
		host, server, err := parseDNSTarget(sv.Target)
		if err != nil {
			return err
		}
		_ = host
		if server != "" {
			h, _, err := net.SplitHostPort(server)
			if err != nil {
				return err
			}
			return r.policy.CheckHost(h)
		}
		return nil
	default:
		return fmt.Errorf("不支持的类型 %q，可选 http/https/tcp/dns", sv.Type)
	}
}

// Do 执行一次探测并返回结果。
func (r *Runner) Do(sv model.Service) model.ServiceResult {
	res := model.ServiceResult{
		ServiceID: sv.ID,
		TS:        time.Now().Unix(),
		CertDays:  -1,
	}
	// 防御性再校验：doHTTP/doTCP 各自只做了自己关心的检查（SSRF、端口），
	// 而 port 范围、https 类型与目标协议是否一致、URL 内嵌凭据这些规则
	// 只在入库路径（ValidateService）跑过。服务定义也可能来自
	// state.json（手工编辑、旧版本数据），不能假设一定经过 API。
	// 这里统一走一遍，避免「入库时合规、运行时不设防」。
	if err := r.ValidateService(sv); err != nil {
		res.OK = false
		res.Detail = err.Error()
		return res
	}
	switch strings.ToLower(sv.Type) {
	case "http", "https":
		r.doHTTP(sv, &res)
	case "tcp":
		r.doTCP(sv, &res)
	case "dns":
		r.doDNS(sv, &res)
	default:
		res.OK = false
		res.Detail = "unsupported type: " + sv.Type
	}
	return res
}

// parseDNSTarget 解析 DNS 探测目标：域名，或 域名@解析服务器[:端口]。
func parseDNSTarget(target string) (host, server string, err error) {
	s := strings.TrimSpace(target)
	if s == "" {
		return "", "", errors.New("DNS 目标不能为空")
	}
	if i := strings.LastIndex(s, "@"); i >= 0 {
		host = strings.TrimSpace(s[:i])
		server = strings.TrimSpace(s[i+1:])
		if server != "" {
			if _, _, e := net.SplitHostPort(server); e != nil {
				server = net.JoinHostPort(server, "53")
			}
			h, port, e := net.SplitHostPort(server)
			if e != nil || h == "" {
				return "", "", errors.New("DNS 服务器格式应为 host 或 host:port")
			}
			// SplitHostPort 只校验"形状"："" / 0 / 99999 都能通过，
			// 到解析时才会静默失败（或者打到 0 端口）。这里校验数值范围。
			n, perr := strconv.Atoi(port)
			if perr != nil || n < 1 || n > 65535 {
				return "", "", errors.New("DNS 服务器端口应在 1–65535 之间")
			}
			server = net.JoinHostPort(h, strconv.Itoa(n))
		}
	} else {
		host = s
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if len(host) == 0 || len(host) > 253 || strings.ContainsAny(host, "/\\ :") {
		return "", "", errors.New("DNS 目标需为域名，例如 example.com 或 example.com@1.1.1.1")
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 {
			return "", "", errors.New("DNS 域名格式无效")
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
				return "", "", errors.New("DNS 域名包含非法字符")
			}
		}
	}
	return host, server, nil
}

func (r *Runner) doDNS(sv model.Service, res *model.ServiceResult) {
	host, server, err := parseDNSTarget(sv.Target)
	if err != nil {
		res.Detail = err.Error()
		return
	}
	resolver := net.DefaultResolver
	if server != "" {
		h, _, _ := net.SplitHostPort(server)
		if err := r.policy.CheckHost(h); err != nil {
			res.Detail = "blocked: " + trimErr(err)
			return
		}
		resolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return r.policy.DialContext(ctx, network, server)
			},
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	start := time.Now()
	addrs, err := resolver.LookupHost(ctx, host)
	res.LatencyMs = int(time.Since(start).Milliseconds())
	if err != nil {
		res.Detail = trimErr(err)
		return
	}
	res.OK = len(addrs) > 0
	detail := strings.Join(addrs, ", ")
	if r := []rune(detail); len(r) > 200 {
		detail = string(r[:200]) + "…"
	}
	res.Detail = detail
}

func (r *Runner) doHTTP(sv model.Service, res *model.ServiceResult) {
	u, err := url.Parse(strings.TrimSpace(sv.Target))
	if err != nil || u.Host == "" {
		res.Detail = "bad url"
		return
	}
	if err := r.policy.CheckURL(u); err != nil {
		res.Detail = "blocked: " + trimErr(err)
		return
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		res.Detail = "bad url"
		return
	}
	req.Header.Set("User-Agent", "motao-rollcall/1.1 (+availability check)")

	// resp 是跳转链的**最后一跳**，它的 TLS 描述的是跳转目标的证书。
	// 目标 301 到 http:// 或别的域名时，用 resp.TLS 取天数会得到
	// 「无 TLS(-1)」或「另一台主机的证书」，证书到期监控静默失效。
	// 因此只记录**配置目标本身**（首跳响应）的 TLS：首跳是 http 时
	// 后续跳转到的 https 证书不能冒充目标证书。
	var targetTLS *tls.ConnectionState
	redirected := false
	cli := *r.client
	baseRedirect := r.client.CheckRedirect
	cli.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) == 1 {
			redirected = true
			if next.Response != nil {
				targetTLS = next.Response.TLS
			}
		}
		if baseRedirect != nil {
			return baseRedirect(next, via)
		}
		// 未配置策略时保留 Go 的默认跳转上限（DefaultCheckRedirect 的行为）。
		if len(via) >= 10 {
			return errors.New("重定向次数过多")
		}
		return nil
	}

	resp, err := cli.Do(req)
	res.LatencyMs = int(time.Since(start).Milliseconds())
	if err != nil {
		res.Detail = trimErr(err)
		return
	}
	defer resp.Body.Close()

	res.Status = resp.StatusCode
	// 仅 2xx 视为健康；401/403 说明服务在线但需授权，也计入在线。
	// 3xx 不再默认健康：跳转已在客户端逐跳校验，最终仍返回 3xx 视为异常。
	res.OK = (resp.StatusCode >= 200 && resp.StatusCode < 300) ||
		resp.StatusCode == 401 || resp.StatusCode == 403

	// 跨主机/跨协议跳转：最终响应不代表配置目标的健康状态，
	// 无论最终状态是否健康都要在详情里可见，避免「配了 A、一直在看 B」。
	redirNote := ""
	if resp.Request != nil {
		from := u.Scheme + "://" + u.Host
		to := resp.Request.URL.Scheme + "://" + resp.Request.URL.Host
		if to != from {
			redirNote = "redirected to " + to
		}
	}
	if !res.OK {
		res.Detail = "status " + strconv.Itoa(resp.StatusCode)
		if redirNote != "" {
			res.Detail += "; " + redirNote
		}
	} else if redirNote != "" {
		res.Detail = redirNote
	}

	// TLS 证书剩余天数。默认握手已校验证书链，能走到这里说明链有效；
	// 仅在显式开启 InsecureTLS 时链未验证，此时天数只作展示。
	// 无跳转时用最终响应；有跳转时只认配置目标（首跳）的证书。
	var tlsState *tls.ConnectionState
	if redirected {
		tlsState = targetTLS
	} else {
		tlsState = resp.TLS
	}
	if tlsState != nil && len(tlsState.PeerCertificates) > 0 {
		res.CertDays = certDaysLeft(tlsState.PeerCertificates[0].NotAfter)
	}
}

// certDaysLeft 返回证书剩余天数：不足一天按 1 天计（向上取整），
// 已过期返回 0；非 TLS 服务由调用方使用 -1 哨兵值。
// 早期实现直接 int(hours/24)，即将过期与刚过期都可能落到 0/-1，
// 与“非 TLS”混淆。
func certDaysLeft(exp time.Time) int {
	days := int(math.Ceil(time.Until(exp).Hours() / 24))
	if days < 0 {
		return 0
	}
	return days
}

func (r *Runner) doTCP(sv model.Service, res *model.ServiceResult) {
	target, err := parseTCPTarget(sv.Target)
	if err != nil {
		res.Detail = err.Error()
		return
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	conn, err := r.policy.DialContext(ctx, "tcp", target)
	res.LatencyMs = int(time.Since(start).Milliseconds())
	if err != nil {
		res.Detail = trimErr(err)
		return
	}
	_ = conn.Close()
	res.OK = true
}

// parseTCPTarget 接受 host:port 或 tcp://host:port，拒绝路径、缺端口与非法端口。
func parseTCPTarget(target string) (string, error) {
	t := strings.TrimSpace(target)
	if strings.HasPrefix(strings.ToLower(t), "tcp://") {
		t = t[len("tcp://"):]
	}
	if t == "" || strings.ContainsAny(t, "/?#") {
		return "", errors.New("TCP 目标需为 host:port，不能包含路径")
	}
	host, port, err := net.SplitHostPort(t)
	if err != nil {
		return "", errors.New("TCP 目标需为 host:port，例如 1.2.3.4:443")
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return "", errors.New("TCP 目标缺少主机名")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", errors.New("TCP 端口必须在 1–65535 之间")
	}
	// 回写规范化后的数字端口：Atoi 接受 "+80"/"080"，
	// 但下游拨号会按原始字符串解释，校验与执行必须一致。
	return net.JoinHostPort(host, strconv.Itoa(n)), nil
}

// trimErr 压缩网络错误信息，避免把完整堆栈写进数据库。
// *url.Error 的 Error() 形如 `Get "https://host/": 真实原因`，
// 直接截断会把最有用的原因切掉，这里先解包只保留底层错误。
func trimErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	s := strings.ReplaceAll(err.Error(), "\n", " ")
	s = strings.TrimSpace(s)
	// 按 rune 截断，避免把中文或转义序列切成半个字符
	r := []rune(s)
	if len(r) > 160 {
		return string(r[:160]) + "…"
	}
	return s
}

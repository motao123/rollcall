// Package probe 实现服务可用性探测。
//
// 探测由**服务端发起**（而非 Agent），因此即使某台 Agent 被攻破，
// 也无法伪造其他节点的服务状态。
package probe

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/motao/probe/internal/model"
)

// Runner 负责按间隔执行探测。
type Runner struct {
	client *http.Client
}

func NewRunner() *Runner {
	return &Runner{
		client: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return http.ErrUseLastResponse
				}
				return nil
			},
			Transport: &http.Transport{
				TLSClientConfig:       &tls.Config{InsecureSkipVerify: true}, // 探测用途：证书有效性单独评估
				DisableKeepAlives:     true,
				ResponseHeaderTimeout: 8 * time.Second,
			},
		},
	}
}

// Do 执行一次探测并返回结果。
func (r *Runner) Do(sv model.Service) model.ServiceResult {
	res := model.ServiceResult{
		ServiceID: sv.ID,
		TS:        time.Now().Unix(),
		CertDays:  -1,
	}
	switch strings.ToLower(sv.Type) {
	case "http", "https":
		r.doHTTP(sv, &res)
	case "tcp":
		r.doTCP(sv, &res)
	default:
		res.OK = false
		res.Detail = "unsupported type: " + sv.Type
	}
	return res
}

func (r *Runner) doHTTP(sv model.Service, res *model.ServiceResult) {
	start := time.Now()
	req, err := http.NewRequest(http.MethodGet, sv.Target, nil)
	if err != nil {
		res.Detail = "bad url"
		return
	}
	req.Header.Set("User-Agent", "motao-probe/1.0 (+availability check)")

	resp, err := r.client.Do(req)
	res.LatencyMs = int(time.Since(start).Milliseconds())
	if err != nil {
		res.Detail = trimErr(err)
		return
	}
	defer resp.Body.Close()

	res.Status = resp.StatusCode
	// 2xx/3xx 视为健康；401/403 说明服务在线但需授权，也计入在线
	res.OK = resp.StatusCode < 400 || resp.StatusCode == 401 || resp.StatusCode == 403
	if !res.OK {
		res.Detail = "status " + itoa(resp.StatusCode)
	}

	// TLS 证书剩余天数
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		exp := resp.TLS.PeerCertificates[0].NotAfter
		res.CertDays = int(time.Until(exp).Hours() / 24)
	}
}

func (r *Runner) doTCP(sv model.Service, res *model.ServiceResult) {
	target := sv.Target
	if u, err := url.Parse("tcp://" + target); err == nil && u.Host != "" {
		target = u.Host
	}
	start := time.Now()
	conn, err := net.DialTimeout("tcp", target, 8*time.Second)
	res.LatencyMs = int(time.Since(start).Milliseconds())
	if err != nil {
		res.Detail = trimErr(err)
		return
	}
	_ = conn.Close()
	res.OK = true
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [8]byte
	p := len(b)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}

// trimErr 压缩网络错误信息，避免把完整堆栈写进数据库。
func trimErr(err error) string {
	s := err.Error()
	if i := strings.Index(s, ": "); i > 0 && len(s) > 90 {
		return s[:i]
	}
	if len(s) > 90 {
		return s[:90]
	}
	return s
}

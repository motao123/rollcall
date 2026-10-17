package web

import (
	"crypto/subtle"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/motao/probe/internal/model"
)

// Version 是构建版本，写入 Prometheus build_info 与设置页。
const Version = "1.1.0"

// ---------- CSV 导出 ----------

// handleExport 提供 CSV 导出，全部需要管理令牌（含运营数据）。
//
//	GET /api/export/assets.csv   资产台账 + 当前状态
//	GET /api/export/alerts.csv   告警历史
//	GET /api/export/metrics.csv?node=<id>&range=1h|6h|24h
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		fail(w, 401, "invalid admin token")
		return
	}
	kind := strings.TrimPrefix(r.URL.Path, "/api/export/")
	switch kind {
	case "assets.csv":
		s.exportAssets(w)
	case "alerts.csv":
		s.exportAlerts(w)
	case "metrics.csv":
		s.exportMetrics(w, r)
	default:
		fail(w, 404, "unknown export: "+kind)
	}
}

// csvWriter 设置下载响应头并返回带 BOM 的 csv.Writer。
// BOM 是必需的：Excel 打开无 BOM 的 UTF-8 CSV 会乱码（中文场景高频踩坑）。
func csvWriter(w http.ResponseWriter, filename string) *csv.Writer {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s-%s.csv"`, filename, time.Now().Format("20060102-1504")))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte{0xEF, 0xBB, 0xBF})
	return csv.NewWriter(w)
}

func (s *Server) exportAssets(w http.ResponseWriter) {
	cw := csvWriter(w, "probe-assets")
	defer cw.Flush()

	_ = cw.Write([]string{
		"名称", "服务商", "位置", "分组", "操作系统", "到期日", "剩余天数",
		"续费价", "币种", "月摊成本USD", "在线", "CPU%", "内存%", "磁盘%", "运行时长",
	})

	var monthSum float64
	for _, v := range s.buildNodeViews() {
		days := 0
		if v.ExpireAt != "" {
			if t, err := time.Parse("2006-01-02", v.ExpireAt); err == nil {
				days = int(time.Until(t).Hours() / 24)
			}
		}
		mv := monthly(v.Price, v.Currency)
		monthSum += mv
		online := "否"
		if v.Online {
			online = "是"
		}
		_ = cw.Write([]string{
			v.Name, v.Provider, v.Region, v.Group, v.Host.OS, v.ExpireAt, itoa(days),
			f2(v.Price), v.Currency, f2(mv), online,
			f1(v.Latest.CPUPct),
			f1(pctF(v.Latest.MemUsed, v.Latest.MemTotal)),
			f1(pctF(v.Latest.DiskUsed, v.Latest.DiskTotal)),
			itoa(int(v.Latest.Uptime/3600)) + "h",
		})
	}
	_ = cw.Write([]string{"合计", "", "", "", "", "", "", "", "", f2(monthSum), "", "", "", "", ""})
}

func (s *Server) exportAlerts(w http.ResponseWriter) {
	cw := csvWriter(w, "probe-alerts")
	defer cw.Flush()

	_ = cw.Write([]string{
		"开始时间", "恢复时间", "状态", "级别", "规则", "节点", "当前值", "说明",
	})
	for _, a := range s.st.Alerts(0) {
		status := "进行中"
		resolved := ""
		if a.ResolvedAt > 0 {
			status = "已恢复"
			resolved = tsFmt(a.ResolvedAt)
		}
		_ = cw.Write([]string{
			tsFmt(a.StartedAt), resolved, status, a.Severity,
			a.RuleName, a.NodeName, f1(a.Value), a.Message,
		})
	}
}

func (s *Server) exportMetrics(w http.ResponseWriter, r *http.Request) {
	nodeID := r.URL.Query().Get("node")
	if nodeID == "" {
		fail(w, 400, "缺少 node 参数")
		return
	}
	node, ok := s.st.Node(nodeID)
	if !ok {
		fail(w, 404, "node not found")
		return
	}
	window := int64(3600)
	switch r.URL.Query().Get("range") {
	case "6h":
		window = 6 * 3600
	case "24h":
		window = 24 * 3600
	}

	pts := s.st.Series(nodeID, time.Now().Unix()-window)
	if len(pts) == 0 {
		fail(w, 404, "该时间范围内没有数据")
		return
	}

	cw := csvWriter(w, "probe-metrics-"+sanitize(node.Name))
	defer cw.Flush()

	_ = cw.Write([]string{
		"时间", "unix", "CPU%", "负载1", "负载5", "负载15",
		"内存已用", "内存总量", "内存%", "Swap已用",
		"磁盘已用", "磁盘总量", "磁盘%",
		"接收字节", "发送字节", "TCP连接", "进程数", "运行秒",
	})
	for _, p := range pts {
		_ = cw.Write([]string{
			tsFmt(p.TS), strconv.FormatInt(p.TS, 10),
			f2(p.CPUPct), f2(p.Load1), f2(p.Load5), f2(p.Load15),
			strconv.FormatUint(p.MemUsed, 10), strconv.FormatUint(p.MemTotal, 10),
			f2(pctF(p.MemUsed, p.MemTotal)), strconv.FormatUint(p.SwapUsed, 10),
			strconv.FormatUint(p.DiskUsed, 10), strconv.FormatUint(p.DiskTotal, 10),
			f2(pctF(p.DiskUsed, p.DiskTotal)),
			strconv.FormatUint(p.NetRx, 10), strconv.FormatUint(p.NetTx, 10),
			strconv.Itoa(p.TCPConn), strconv.Itoa(p.Procs), strconv.FormatUint(p.Uptime, 10),
		})
	}
}

// ---------- Prometheus ----------

// handleMetrics 以 Prometheus exposition 格式暴露指标。
//
// 鉴权：X-Admin-Token 头，或 Basic Auth（用户名任意、密码为管理令牌）。
// Prometheus 抓取配置示例：
//
//	scrape_configs:
//	  - job_name: probe
//	    static_configs: [{ targets: ["host:19080"] }]
//	    basic_auth: { username: probe, password: "<admin.token 内容>" }
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if !s.metricsAuthOK(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="Probe metrics"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var b strings.Builder
	now := time.Now().Unix()

	help := func(name, text, typ string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, text, name, typ)
	}
	gauge := func(name string, labels []string, v float64) {
		if len(labels) == 0 {
			fmt.Fprintf(&b, "%s %s\n", name, f(v))
			return
		}
		fmt.Fprintf(&b, "%s{%s} %s\n", name, joinLabels(labels), f(v))
	}

	// 构建信息
	help("probe_build_info", "Build information.", "gauge")
	fmt.Fprintf(&b, "probe_build_info{version=%q,go=%q} 1\n", Version, runtime.Version())

	// 汇总
	views := s.buildNodeViews()
	online := 0
	for _, v := range views {
		if v.Online {
			online++
		}
	}
	help("probe_nodes_total", "Total registered nodes.", "gauge")
	gauge("probe_nodes_total", nil, float64(len(views)))
	help("probe_nodes_online", "Nodes currently online.", "gauge")
	gauge("probe_nodes_online", nil, float64(online))
	help("probe_alerts_active", "Currently active alerts.", "gauge")
	gauge("probe_alerts_active", nil, float64(s.st.ActiveAlertCount()))

	svcs := s.st.Services()
	svcDown := 0
	for _, sv := range svcs {
		_, _, ok, has := s.st.ServiceLast(sv.ID)
		if has && !ok {
			svcDown++
		}
	}
	help("probe_services_total", "Total service probes.", "gauge")
	gauge("probe_services_total", nil, float64(len(svcs)))
	help("probe_services_down", "Service probes currently failing.", "gauge")
	gauge("probe_services_down", nil, float64(svcDown))

	// 节点指标
	nodeMetrics := []struct {
		name string
		help string
		typ  string
		get  func(v model.NodeView) float64
	}{
		{"probe_node_online", "1 if node reported recently, else 0.", "gauge",
			func(v model.NodeView) float64 { return b2f(v.Online) }},
		{"probe_node_cpu_percent", "CPU usage percent.", "gauge",
			func(v model.NodeView) float64 { return v.Latest.CPUPct }},
		{"probe_node_memory_percent", "Memory usage percent.", "gauge",
			func(v model.NodeView) float64 { return pctF(v.Latest.MemUsed, v.Latest.MemTotal) }},
		{"probe_node_disk_percent", "Root disk usage percent.", "gauge",
			func(v model.NodeView) float64 { return pctF(v.Latest.DiskUsed, v.Latest.DiskTotal) }},
		{"probe_node_load1", "1-minute load average.", "gauge",
			func(v model.NodeView) float64 { return v.Latest.Load1 }},
		{"probe_node_load5", "5-minute load average.", "gauge",
			func(v model.NodeView) float64 { return v.Latest.Load5 }},
		{"probe_node_load15", "15-minute load average.", "gauge",
			func(v model.NodeView) float64 { return v.Latest.Load15 }},
		{"probe_node_uptime_seconds", "Node uptime in seconds.", "gauge",
			func(v model.NodeView) float64 { return float64(v.Latest.Uptime) }},
		{"probe_node_tcp_connections", "Established TCP connections.", "gauge",
			func(v model.NodeView) float64 { return float64(v.Latest.TCPConn) }},
		{"probe_node_processes", "Running processes.", "gauge",
			func(v model.NodeView) float64 { return float64(v.Latest.Procs) }},
		{"probe_node_network_receive_bytes_total", "Cumulative received bytes.", "counter",
			func(v model.NodeView) float64 { return float64(v.Latest.NetRx) }},
		{"probe_node_network_transmit_bytes_total", "Cumulative transmitted bytes.", "counter",
			func(v model.NodeView) float64 { return float64(v.Latest.NetTx) }},
		{"probe_node_last_report_age_seconds", "Seconds since last report.", "gauge",
			func(v model.NodeView) float64 {
				if v.LastSeen == 0 {
					return -1
				}
				return float64(now - v.LastSeen)
			}},
	}
	for _, m := range nodeMetrics {
		help(m.name, m.help, m.typ)
		for _, v := range views {
			gauge(m.name, []string{"id=" + q(v.ID), "name=" + q(v.Name)}, m.get(v))
		}
	}

	// 服务探测
	svcMetrics := []struct {
		name string
		help string
		get  func(ok bool, lat int, cert int, up float64) float64
	}{
		{"probe_service_up", "1 if last probe succeeded.", func(ok bool, lat, cert int, up float64) float64 { return b2f(ok) }},
		{"probe_service_latency_milliseconds", "Last probe latency in ms.", func(ok bool, lat, cert int, up float64) float64 { return float64(lat) }},
		{"probe_service_certificate_days_remaining", "TLS cert days left (-1 = n/a).", func(ok bool, lat, cert int, up float64) float64 { return float64(cert) }},
		{"probe_service_uptime_percent_24h", "24h availability percent.", func(ok bool, lat, cert int, up float64) float64 { return up }},
	}
	for _, m := range svcMetrics {
		help(m.name, m.help, "gauge")
		for _, sv := range svcs {
			_, lat, ok, cert, has := s.st.ServiceLastFull(sv.ID)
			hist := s.st.ServiceSeries(sv.ID)
			upN, tot := 0, 0
			for _, h := range hist {
				tot++
				if h[2] == 1 {
					upN++
				}
			}
			up := 0.0
			if tot > 0 {
				up = float64(upN) / float64(tot) * 100
			}
			if !has {
				ok, lat, cert = false, 0, -1
			}
			gauge(m.name, []string{"id=" + q(sv.ID), "name=" + q(sv.Name)}, m.get(ok, lat, cert, up))
		}
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = io.WriteString(w, b.String())
}

// metricsAuthOK 接受 X-Admin-Token 头或 Basic Auth 密码为管理令牌。
func (s *Server) metricsAuthOK(r *http.Request) bool {
	if s.adminToken == "" {
		return false
	}
	if t := r.Header.Get("X-Admin-Token"); t != "" {
		return subtle.ConstantTimeCompare([]byte(t), []byte(s.adminToken)) == 1
	}
	if _, pw, ok := r.BasicAuth(); ok {
		return subtle.ConstantTimeCompare([]byte(pw), []byte(s.adminToken)) == 1
	}
	return false
}

// ---------- 小工具 ----------

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func f(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func f1(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }

func f2(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

func tsFmt(ts int64) string {
	if ts <= 0 {
		return ""
	}
	return time.Unix(ts, 0).Format("2006-01-02 15:04:05")
}

// sanitize 去掉文件名不安全字符。
func sanitize(s string) string {
	r := strings.NewReplacer("/", "-", "\\", "-", ":", "-", "*", "-", "?", "-", "\"", "-", "<", "-", ">", "-", "|", "-", " ", "-")
	return r.Replace(s)
}

// q 转义 Prometheus 标签值。
func q(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return `"` + s + `"`
}

func joinLabels(pairs []string) string {
	return strings.Join(pairs, ",")
}

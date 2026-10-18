package web

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/motao/rollcall/internal/model"
	"github.com/motao/rollcall/internal/store"
	"github.com/motao/rollcall/internal/version"
)

// ---------- CSV 导出 ----------

// handleExport 提供 CSV 导出，全部需要管理令牌（含运营数据）。
//
//	GET /api/export/assets.csv   资产台账 + 当前状态
//	GET /api/export/alerts.csv   告警历史
//	GET /api/export/metrics.csv?node=<id>&range=1h|6h|24h|7d|30d|365d
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

// csvSafe 阻断 CSV 公式注入。
//
// Excel / WPS 会把以 = + - @ 开头的单元格当公式求值，
// 以 Tab(0x09) / CR(0x0D) 开头则可能被当作 DDE 载荷
// （如 =cmd|'/c calc'!A1）。这些字段并非全部可信：
// 被攻破的 Agent 可以自报任意 host.os / host.hostname，
// 经 assets.csv 直达管理员的 Excel —— 那就不再是"只能伪造指标"，
// 而是能在管理员机器上执行命令。
//
// 处理方式是在前缀加单引号，Excel 会将其视为文本：
// 不截断、不转义其余内容，正常数据零影响。
func csvSafe(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

// csvBuffer 在内存中构建带 BOM 的 CSV。
// 先渲染后写出，避免编码失败时向客户端留下半截文件。
// BOM 是必需的：Excel 打开无 BOM 的 UTF-8 CSV 会乱码（中文场景高频踩坑）。
func csvBuffer() (*bytes.Buffer, *csv.Writer) {
	buf := &bytes.Buffer{}
	buf.Write([]byte{0xEF, 0xBB, 0xBF})
	return buf, csv.NewWriter(buf)
}

// flushCSV 结束编码并写出下载响应；编码失败时返回 500 而非残缺文件。
func flushCSV(w http.ResponseWriter, filename string, buf *bytes.Buffer, cw *csv.Writer) {
	cw.Flush()
	if err := cw.Error(); err != nil {
		fail(w, 500, "CSV 生成失败: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s-%s.csv"`, filename, time.Now().Format("20060102-1504")))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) exportAssets(w http.ResponseWriter) {
	buf, cw := csvBuffer()

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
		mv := monthly(v.Price, v.Currency, s.cnyRate)
		monthSum += mv
		online := "否"
		if v.Online {
			online = "是"
		}
		_ = cw.Write([]string{
			csvSafe(v.Name), csvSafe(v.Provider), csvSafe(v.Region), csvSafe(v.Group),
			csvSafe(v.Host.OS), v.ExpireAt, itoa(days),
			f2(v.Price), v.Currency, f2(mv), online,
			f1(v.Latest.CPUPct),
			f1(pctF(v.Latest.MemUsed, v.Latest.MemTotal)),
			f1(pctF(v.Latest.DiskUsed, v.Latest.DiskTotal)),
			itoa(int(v.Latest.Uptime/3600)) + "h",
		})
	}
	_ = cw.Write([]string{"合计", "", "", "", "", "", "", "", "", f2(monthSum), "", "", "", "", ""})
	flushCSV(w, "rollcall-assets", buf, cw)
}

func (s *Server) exportAlerts(w http.ResponseWriter) {
	buf, cw := csvBuffer()

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
			csvSafe(a.RuleName), csvSafe(a.NodeName), f1(a.Value), csvSafe(a.Message),
		})
	}
	flushCSV(w, "rollcall-alerts", buf, cw)
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
	window, resolution, ok := historyRange(r.URL.Query().Get("range"))
	if !ok {
		fail(w, 400, "未知的时间范围")
		return
	}
	since := time.Now().Unix() - window
	buf, cw := csvBuffer()

	if resolution == store.HistoryResolutionRaw {
		pts := s.st.Series(nodeID, since)
		if len(pts) == 0 {
			fail(w, 404, "该时间范围内没有数据")
			return
		}
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
		flushCSV(w, "rollcall-metrics-"+sanitize(node.Name), buf, cw)
		return
	}

	// 聚合区间：数值是桶内平均/增量，不能与原始累计计数器混用同一组列。
	pts := s.st.HistorySeries(nodeID, resolution, since)
	if len(pts) == 0 {
		fail(w, 404, "该时间范围内没有数据")
		return
	}
	_ = cw.Write([]string{
		"时间", "unix", "分辨率", "CPU%", "CPU峰值",
		"内存已用", "内存总量", "内存%", "磁盘已用", "磁盘总量", "磁盘%",
		"负载1", "接收增量字节", "发送增量字节", "样本数",
	})
	for _, p := range pts {
		_ = cw.Write([]string{
			tsFmt(p.TS), strconv.FormatInt(p.TS, 10), resolution,
			f2(p.CPUPct), f2(p.CPUMax),
			strconv.FormatUint(p.MemUsed, 10), strconv.FormatUint(p.MemTotal, 10), f2(p.MemPct),
			strconv.FormatUint(p.DiskUsed, 10), strconv.FormatUint(p.DiskTotal, 10), f2(p.DiskPct),
			f2(p.Load1),
			strconv.FormatUint(p.NetRxDelta, 10), strconv.FormatUint(p.NetTxDelta, 10),
			strconv.FormatUint(uint64(p.Count), 10),
		})
	}
	flushCSV(w, "rollcall-metrics-"+sanitize(node.Name), buf, cw)
}

// ---------- Prometheus ----------

// handleMetrics 以 Prometheus exposition 格式暴露指标。
//
// 鉴权：X-Admin-Token 头，或 Basic Auth（用户名任意、密码为管理令牌）。
// Prometheus 抓取配置示例：
//
//	scrape_configs:
//	  - job_name: rollcall
//	    static_configs: [{ targets: ["host:19080"] }]
//	    basic_auth: { username: rollcall, password: "<admin.token 内容>" }
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if !s.metricsAuthOK(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="Rollcall metrics"`)
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
	help("rollcall_build_info", "Build information.", "gauge")
	fmt.Fprintf(&b, "rollcall_build_info{version=%q,go=%q} 1\n", version.Version, runtime.Version())

	// 汇总
	views := s.buildNodeViews()
	online := 0
	for _, v := range views {
		if v.Online {
			online++
		}
	}
	help("rollcall_nodes_total", "Total registered nodes.", "gauge")
	gauge("rollcall_nodes_total", nil, float64(len(views)))
	help("rollcall_nodes_online", "Nodes currently online.", "gauge")
	gauge("rollcall_nodes_online", nil, float64(online))
	help("rollcall_alerts_active", "Currently active alerts.", "gauge")
	gauge("rollcall_alerts_active", nil, float64(s.st.ActiveAlertCount()))

	// 通知队列
	ns := s.st.NotifyStats()
	help("rollcall_notify_queue_pending", "Notifications waiting to be delivered or retried.", "gauge")
	gauge("rollcall_notify_queue_pending", nil, float64(ns.Pending))
	help("rollcall_notify_sent_total", "Notifications delivered successfully (at least one channel).", "counter")
	gauge("rollcall_notify_sent_total", nil, float64(ns.Sent))
	help("rollcall_notify_failed_total", "Notifications abandoned after all retries.", "counter")
	gauge("rollcall_notify_failed_total", nil, float64(ns.Failed))
	help("rollcall_notify_retries_total", "Notification delivery retries scheduled.", "counter")
	gauge("rollcall_notify_retries_total", nil, float64(ns.Retries))
	help("rollcall_notify_suppressed_total", "Notifications suppressed by silence or disabled config.", "counter")
	gauge("rollcall_notify_suppressed_total", nil, float64(ns.Suppressed))
	help("rollcall_notify_last_success_timestamp_seconds", "Unix timestamp of the last successful delivery (0 = never).", "gauge")
	gauge("rollcall_notify_last_success_timestamp_seconds", nil, float64(ns.LastSuccess))
	help("rollcall_notify_silences_active", "Active notification silences (global or per-rule).", "gauge")
	gauge("rollcall_notify_silences_active", nil, float64(len(s.st.Silences(now))))

	svcs := s.st.Services()
	svcDown := 0
	for _, sv := range svcs {
		_, _, ok, has := s.st.ServiceLast(sv.ID)
		if has && !ok {
			svcDown++
		}
	}
	help("rollcall_services_total", "Total service probes.", "gauge")
	gauge("rollcall_services_total", nil, float64(len(svcs)))
	help("rollcall_services_down", "Service probes currently failing.", "gauge")
	gauge("rollcall_services_down", nil, float64(svcDown))

	// 节点指标
	nodeMetrics := []struct {
		name string
		help string
		typ  string
		get  func(v model.NodeView) float64
	}{
		{"rollcall_node_online", "1 if node reported recently, else 0.", "gauge",
			func(v model.NodeView) float64 { return b2f(v.Online) }},
		{"rollcall_node_cpu_percent", "CPU usage percent.", "gauge",
			func(v model.NodeView) float64 { return v.Latest.CPUPct }},
		{"rollcall_node_memory_percent", "Memory usage percent.", "gauge",
			func(v model.NodeView) float64 { return pctF(v.Latest.MemUsed, v.Latest.MemTotal) }},
		{"rollcall_node_disk_percent", "Root disk usage percent.", "gauge",
			func(v model.NodeView) float64 { return pctF(v.Latest.DiskUsed, v.Latest.DiskTotal) }},
		{"rollcall_node_load1", "1-minute load average.", "gauge",
			func(v model.NodeView) float64 { return v.Latest.Load1 }},
		{"rollcall_node_load5", "5-minute load average.", "gauge",
			func(v model.NodeView) float64 { return v.Latest.Load5 }},
		{"rollcall_node_load15", "15-minute load average.", "gauge",
			func(v model.NodeView) float64 { return v.Latest.Load15 }},
		{"rollcall_node_uptime_seconds", "Node uptime in seconds.", "gauge",
			func(v model.NodeView) float64 { return float64(v.Latest.Uptime) }},
		{"rollcall_node_tcp_connections", "Established TCP connections.", "gauge",
			func(v model.NodeView) float64 { return float64(v.Latest.TCPConn) }},
		{"rollcall_node_processes", "Running processes.", "gauge",
			func(v model.NodeView) float64 { return float64(v.Latest.Procs) }},
		{"rollcall_node_network_receive_bytes_total", "Cumulative received bytes.", "counter",
			func(v model.NodeView) float64 { return float64(v.Latest.NetRx) }},
		{"rollcall_node_network_transmit_bytes_total", "Cumulative transmitted bytes.", "counter",
			func(v model.NodeView) float64 { return float64(v.Latest.NetTx) }},
		{"rollcall_node_last_report_age_seconds", "Seconds since last report.", "gauge",
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
	// 历史只读取一次：旧实现每个指标都复制一遍整段环形缓冲。
	type svcData struct {
		sv   *model.Service
		ok   bool
		has  bool
		lat  int
		cert int
		up   float64
		upOK bool
	}
	svcRows := make([]svcData, 0, len(svcs))
	for _, sv := range svcs {
		_, lat, ok, cert, has := s.st.ServiceLastFull(sv.ID)
		if !has {
			ok, lat, cert = false, 0, -1
		}
		up, upOK := serviceUptime24(s.st.ServiceSeries(sv.ID), now)
		svcRows = append(svcRows, svcData{sv: sv, ok: ok, has: has, lat: lat, cert: cert, up: up, upOK: upOK})
	}

	svcMetrics := []struct {
		name       string
		help       string
		needUptime bool
		get        func(ok bool, lat, cert int, up float64) float64
	}{
		{"rollcall_service_up", "1 if last probe succeeded.", false, func(ok bool, lat, cert int, up float64) float64 { return b2f(ok) }},
		{"rollcall_service_latency_milliseconds", "Last probe latency in ms.", false, func(ok bool, lat, cert int, up float64) float64 { return float64(lat) }},
		{"rollcall_service_certificate_days_remaining", "TLS cert days left (-1 = n/a).", false, func(ok bool, lat, cert int, up float64) float64 { return float64(cert) }},
		{"rollcall_service_uptime_percent_24h", "24h availability percent.", true, func(ok bool, lat, cert int, up float64) float64 { return up }},
	}
	for _, m := range svcMetrics {
		help(m.name, m.help, "gauge")
		for _, d := range svcRows {
			// 没有任何 24h 内样本时跳过该 gauge，避免输出误导性的 0%
			if m.needUptime && !d.upOK {
				continue
			}
			gauge(m.name, []string{"id=" + q(d.sv.ID), "name=" + q(d.sv.Name)}, m.get(d.ok, d.lat, d.cert, d.up))
		}
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = io.WriteString(w, b.String())
}

// metricsAuthOK 接受 X-Admin-Token，或 Basic 密码为管理令牌（用户名任意）；
// 启用 -auth 时也接受面板凭据，方便同一套 Basic Auth 同时抓指标。
func (s *Server) metricsAuthOK(r *http.Request) bool {
	return s.adminTokenOK(r) || s.panelBasicOK(r)
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

// sanitize 清理用于 Content-Disposition 文件名的字符串。
// 除文件系统非法字符外，必须一并去掉 CR/LF 与控制字符，
// 否则节点名可被用来注入响应头（node.Name 经管理 API 可控）。
func sanitize(s string) string {
	r := strings.NewReplacer("/", "-", "\\", "-", ":", "-", "*", "-", "?", "-", "\"", "-", "<", "-", ">", "-", "|", "-", " ", "-", "\r", "", "\n", "", "\t", "-")
	out := r.Replace(s)
	// 去除其余控制字符（0x00–0x1F），避免任何头注入残留
	return strings.Map(func(c rune) rune {
		if c < 0x20 || c == 0x7f {
			return -1
		}
		return c
	}, out)
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

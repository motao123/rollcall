package web

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/store"
	"github.com/motao123/rollcall/internal/version"
)

// ---------- CSV 导出 ----------

// handleExport 提供 CSV 导出，全部需要管理令牌（含运营数据）。
//
//	GET /api/export/assets.csv   资产台账 + 当前状态
//	GET /api/export/alerts.csv   告警历史
//	GET /api/export/metrics.csv?node=<id>&range=1h|6h|24h|7d|30d|365d
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	kind := strings.TrimPrefix(r.URL.Path, "/api/export/")
	switch kind {
	case "assets.csv":
		s.exportAssets(w)
	case "alerts.csv":
		s.exportAlerts(w)
	case "metrics.csv":
		s.exportMetrics(w, r)
	case "history.csv":
		s.exportHistory(w, r)
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
		contentDisposition(filename+"-"+time.Now().Format("20060102-1504")+".csv"))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	_, _ = w.Write(buf.Bytes())
}

// contentDisposition 生成兼容非 ASCII 文件名的下载头。
//
// 节点名普遍是中文（腾讯云-sg、武汉-独服），而 `filename="..."` 只允许
// ASCII 可见字符：直接塞 UTF-8 会被部分浏览器截断成乱码或空文件名。
// 按 RFC 6266/5987 同时给出 ASCII 回退名与 filename*=UTF-8” 形式。
func contentDisposition(name string) string {
	var ascii strings.Builder
	for _, r := range name {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			ascii.WriteByte('_')
			continue
		}
		ascii.WriteRune(r)
	}
	return fmt.Sprintf("attachment; filename=%q; filename*=UTF-8''%s",
		ascii.String(), encodeRFC5987(name))
}

// encodeRFC5987 按 RFC 5987 的 attr-char 集合编码。
// url.PathEscape 是路径段编码：它保留 = @ : 等字符，而这些在
// filename* 中不合法，部分浏览器会把整个文件名当解析失败。
func encodeRFC5987(s string) string {
	const attrChars = "!#$&+-.^_`|~"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			strings.IndexByte(attrChars, c) >= 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func (s *Server) exportAssets(w http.ResponseWriter) {
	buf, cw := csvBuffer()

	_ = cw.Write([]string{
		"名称", "服务商", "位置", "分组", "操作系统", "到期日", "剩余天数",
		"续费价", "币种", "月摊成本USD", "在线", "CPU%", "内存%", "磁盘%", "运行时长",
	})

	var monthSum float64
	for _, v := range s.buildNodeViews() {
		// 与仪表盘共用 daysUntil：它按本地日历日计算并四舍五入，
		// 避免 UTC 解析 + 截断把「今天到期」和「还有 23 小时」都算成 0 天。
		// 无法解析时输出空值：填 0 会被读成「今天到期」。
		days, daysOK := daysUntil(v.ExpireAt)
		daysStr := ""
		if daysOK {
			daysStr = itoa(days)
		}
		mv := monthly(v.Price, v.Currency, s.cnyRate)
		monthSum += mv
		online := "否"
		if v.Online {
			online = "是"
		}
		_ = cw.Write([]string{
			csvSafe(v.Name), csvSafe(v.Provider), csvSafe(v.Region), csvSafe(v.Group),
			csvSafe(v.Host.OS), v.ExpireAt, daysStr,
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
		flushCSV(w, "rollcall-metrics-"+safeFileName(node.Name), buf, cw)
		return
	}

	// 聚合区间：数值是桶内平均/增量，不能与原始累计计数器混用同一组列。
	pts := s.st.HistorySeries(nodeID, resolution, since)
	if len(pts) == 0 {
		fail(w, 404, "该时间范围内没有数据")
		return
	}
	// p50/p95 追加在**末尾**而不是紧跟 CPU峰值：导出文件可能被脚本按列位置解析，
	// 插在中间会让其后所有列右移。空值（无直方图的老桶）留空而不是写 0 ——
	// 0 会被误读成"CPU 使用率为 0"，空才表示"该桶没有摘要"。
	_ = cw.Write([]string{
		"时间", "unix", "分辨率", "CPU%", "CPU峰值",
		"内存已用", "内存总量", "内存%", "磁盘已用", "磁盘总量", "磁盘%",
		"负载1", "接收增量字节", "发送增量字节", "样本数",
		"CPU p50", "CPU p95",
	})
	optF2 := func(v float64) string {
		if v <= 0 {
			return ""
		}
		return f2(v)
	}
	for _, p := range pts {
		_ = cw.Write([]string{
			tsFmt(p.TS), strconv.FormatInt(p.TS, 10), resolution,
			f2(p.CPUPct), f2(p.CPUMax),
			strconv.FormatUint(p.MemUsed, 10), strconv.FormatUint(p.MemTotal, 10), f2(p.MemPct),
			strconv.FormatUint(p.DiskUsed, 10), strconv.FormatUint(p.DiskTotal, 10), f2(p.DiskPct),
			f2(p.Load1),
			strconv.FormatUint(p.NetRxDelta, 10), strconv.FormatUint(p.NetTxDelta, 10),
			strconv.FormatUint(uint64(p.Count), 10),
			optF2(p.CPUP50), optF2(p.CPUP95),
		})
	}
	flushCSV(w, "rollcall-metrics-"+safeFileName(node.Name), buf, cw)
}

// exportHistory 导出全机群的历史归档（CSV）。
//
//	GET /api/export/history.csv?range=24h|7d|30d|365d
//
// 与单节点 metrics.csv 不同，这里固定使用聚合层并覆盖全部节点，
// 适合离线分析/长期归档：24h 用 5m 桶，更长区间用 1h 桶，
// 避免一次导出几十万行把浏览器和服务端一起拖慢。
func (s *Server) exportHistory(w http.ResponseWriter, r *http.Request) {
	rng := r.URL.Query().Get("range")
	res := store.HistoryResolution5m
	window := int64(24 * 3600)
	switch rng {
	case "", "24h":
		rng = "24h"
	case "7d":
		res = store.HistoryResolution1h
		window = 7 * 24 * 3600
	case "30d":
		res = store.HistoryResolution1h
		window = 30 * 24 * 3600
	case "365d":
		res = store.HistoryResolution1h
		window = 365 * 24 * 3600
	default:
		fail(w, 400, "未知的时间范围")
		return
	}
	since := time.Now().Unix() - window
	buf, cw := csvBuffer()
	_ = cw.Write([]string{
		"节点ID", "节点名", "时间", "unix", "分辨率", "CPU%", "CPU峰值",
		"内存%", "磁盘%", "负载1", "接收增量字节", "发送增量字节", "样本数",
	})
	for _, n := range s.st.Nodes() {
		for _, p := range s.st.HistorySeries(n.ID, res, since) {
			if p.Count == 0 {
				continue
			}
			_ = cw.Write([]string{
				csvSafe(n.ID), csvSafe(n.Name), tsFmt(p.TS), strconv.FormatInt(p.TS, 10), res,
				f2(p.CPUPct), f2(p.CPUMax), f2(p.MemPct), f2(p.DiskPct), f2(p.Load1),
				strconv.FormatUint(p.NetRxDelta, 10), strconv.FormatUint(p.NetTxDelta, 10),
				strconv.FormatUint(uint64(p.Count), 10),
			})
		}
	}
	flushCSV(w, "rollcall-history-"+rng, buf, cw)
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
	// 必须用 Prometheus 的 q 转义：%q 会输出 \t、\xNN 等 Go 转义，
	// Prometheus 文本解析器不接受，整个 /metrics 会抓取失败。
	fmt.Fprintf(&b, "rollcall_build_info{version=%s,go=%s} 1\n", q(version.Version), q(runtime.Version()))

	// HTTP 请求量、状态码与延迟分布。
	// route 标签使用 ServeMux pattern，不展开节点 ID 等路径参数。
	hs := s.http.snapshot()
	help("rollcall_http_requests_total", "HTTP requests by route pattern, method and status code.", "counter")
	for _, row := range hs.requests {
		gauge("rollcall_http_requests_total", []string{
			"route=" + q(row.key.route), "method=" + q(row.key.method),
			"status=" + strconv.Itoa(row.key.status),
		}, float64(row.val))
	}
	help("rollcall_http_request_duration_seconds", "HTTP request latency in seconds by route pattern (SSE streams excluded).", "histogram")
	for _, row := range hs.durs {
		var cum uint64
		for i, le := range httpLatencyBuckets {
			cum += row.val.buckets[i]
			fmt.Fprintf(&b, "rollcall_http_request_duration_seconds_bucket{route=%s,method=%s,le=%s} %d\n",
				q(row.key.route), q(row.key.method), q(f(le)), cum)
		}
		cum += row.val.inf
		fmt.Fprintf(&b, "rollcall_http_request_duration_seconds_bucket{route=%s,method=%s,le=\"+Inf\"} %d\n",
			q(row.key.route), q(row.key.method), cum)
		fmt.Fprintf(&b, "rollcall_http_request_duration_seconds_sum{route=%s,method=%s} %s\n",
			q(row.key.route), q(row.key.method), f(row.val.sum))
		fmt.Fprintf(&b, "rollcall_http_request_duration_seconds_count{route=%s,method=%s} %d\n",
			q(row.key.route), q(row.key.method), row.val.count)
	}
	help("rollcall_http_inflight_requests", "HTTP requests currently being served.", "gauge")
	gauge("rollcall_http_inflight_requests", nil, float64(hs.inflight))
	help("rollcall_sse_subscribers", "Active SSE stream subscribers (limit 256).", "gauge")
	gauge("rollcall_sse_subscribers", nil, float64(s.events.subscriberCount()))
	// 汇总
	// 只统计状态，不构造完整视图：/metrics 每次抓取都会跑，
	// buildNodeViews 会为每个节点复制最近 20 分钟的原始序列并建告警映射，
	// 这些在抓取路径上完全用不到（节点多时是 O(n × 点数) 的额外分配）。
	nodes := s.st.Nodes()
	online, stale, offline, noData := 0, 0, 0, 0
	for _, n := range nodes {
		_, ts, _ := s.st.Latest(n.ID)
		switch store.NodeState(ts, now) {
		case model.NodeStateOnline:
			online++
		case model.NodeStateStale:
			stale++
		case model.NodeStateOffline:
			offline++
		case model.NodeStateNoData:
			noData++
		}
	}
	help("rollcall_nodes_total", "Total registered nodes.", "gauge")
	gauge("rollcall_nodes_total", nil, float64(len(nodes)))
	help("rollcall_nodes_online", "Nodes reporting within the online window.", "gauge")
	gauge("rollcall_nodes_online", nil, float64(online))
	help("rollcall_nodes_stale", "Nodes whose last report is late but not yet offline.", "gauge")
	gauge("rollcall_nodes_stale", nil, float64(stale))
	help("rollcall_nodes_offline", "Nodes without a report past the stale window.", "gauge")
	gauge("rollcall_nodes_offline", nil, float64(offline))
	help("rollcall_nodes_no_data", "Registered nodes whose agent has never reported.", "gauge")
	gauge("rollcall_nodes_no_data", nil, float64(noData))
	help("rollcall_alerts_active", "Currently active alerts.", "gauge")
	gauge("rollcall_alerts_active", nil, float64(s.st.ActiveAlertCount()))

	// 状态快照落盘
	ss := s.st.SaveStats()
	help("rollcall_save_total", "State snapshot save attempts.", "counter")
	gauge("rollcall_save_total", nil, float64(ss.Count))
	help("rollcall_save_failures_total", "State snapshot saves that failed.", "counter")
	gauge("rollcall_save_failures_total", nil, float64(ss.Failures))
	help("rollcall_save_duration_seconds", "Duration of the last successful state snapshot save.", "gauge")
	gauge("rollcall_save_duration_seconds", nil, ss.LastDurationSeconds)
	help("rollcall_save_last_success_timestamp_seconds", "Unix timestamp of the last successful state save (0 = never).", "gauge")
	gauge("rollcall_save_last_success_timestamp_seconds", nil, float64(ss.LastSuccess))
	// 分段耗时：定位快照瓶颈用（copy=锁内深拷贝 encode=流式 JSON+gzip sync=fsync）
	help("rollcall_save_copy_seconds", "Duration of the deep copy phase of the last successful save.", "gauge")
	gauge("rollcall_save_copy_seconds", nil, ss.LastCopySeconds)
	help("rollcall_save_marshal_seconds", "Duration of the JSON encoding phase of the last successful save.", "gauge")
	gauge("rollcall_save_marshal_seconds", nil, ss.LastMarshalSeconds)
	help("rollcall_save_encode_seconds", "Duration of the gzip compression phase of the last successful save.", "gauge")
	gauge("rollcall_save_encode_seconds", nil, ss.LastEncodeSeconds)
	help("rollcall_save_write_seconds", "Duration of the write+rename phase of the last successful save.", "gauge")
	gauge("rollcall_save_write_seconds", nil, ss.LastWriteSeconds)
	help("rollcall_save_bytes", "Size of the last compressed state snapshot in bytes (config + current state only; history lives in series shards).", "gauge")
	gauge("rollcall_save_bytes", nil, float64(ss.LastBytes))
	help("rollcall_save_series_seconds", "Duration of writing history shards in the last successful save.", "gauge")
	gauge("rollcall_save_series_seconds", nil, ss.LastSeriesSeconds)
	help("rollcall_save_series_bytes", "Bytes written to history shards in the last successful save.", "gauge")
	gauge("rollcall_save_series_bytes", nil, float64(ss.LastSeriesBytes))
	help("rollcall_save_series_files", "Number of history shard files written in the last successful save (incremental: only dirty nodes).", "gauge")
	gauge("rollcall_save_series_files", nil, float64(ss.LastSeriesFiles))

	// 自动备份：未启用时也输出 0，便于用 absent() 之外的规则统一判断。
	enabled := 0.0
	var autoLast, autoNext int64
	var autoRuns, autoFailures, autoSize, autoFiles, autoBytes int64
	if s.autoBackup != nil {
		enabled = 1
		bs := s.autoBackup.Status()
		autoLast, autoNext = bs.LastAt, bs.NextAt
		autoRuns, autoFailures = bs.Runs, bs.Failures
		autoSize = bs.LastSize
		autoFiles = int64(len(bs.Files))
		autoBytes = bs.TotalBytes
	}
	help("rollcall_backup_auto_enabled", "Automatic encrypted backup enabled (1/0).", "gauge")
	gauge("rollcall_backup_auto_enabled", nil, enabled)
	help("rollcall_backup_auto_runs_total", "Automatic backup attempts.", "counter")
	gauge("rollcall_backup_auto_runs_total", nil, float64(autoRuns))
	help("rollcall_backup_auto_failures_total", "Automatic backup attempts that failed.", "counter")
	gauge("rollcall_backup_auto_failures_total", nil, float64(autoFailures))
	help("rollcall_backup_auto_last_run_timestamp_seconds", "Unix timestamp of the last automatic backup attempt (0 = never).", "gauge")
	gauge("rollcall_backup_auto_last_run_timestamp_seconds", nil, float64(autoLast))
	help("rollcall_backup_auto_next_run_timestamp_seconds", "Unix timestamp of the next scheduled automatic backup (0 = disabled).", "gauge")
	gauge("rollcall_backup_auto_next_run_timestamp_seconds", nil, float64(autoNext))
	help("rollcall_backup_auto_last_size_bytes", "Size of the last automatic backup file.", "gauge")
	gauge("rollcall_backup_auto_last_size_bytes", nil, float64(autoSize))
	help("rollcall_backup_auto_files", "Automatic backup files currently retained.", "gauge")
	gauge("rollcall_backup_auto_files", nil, float64(autoFiles))
	help("rollcall_backup_auto_bytes", "Total bytes of retained automatic backup files.", "gauge")
	gauge("rollcall_backup_auto_total_bytes", nil, float64(autoBytes))

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
	help("rollcall_notify_rejected_total", "Notifications rejected because the outbox queue was full.", "counter")
	gauge("rollcall_notify_rejected_total", nil, float64(ns.Rejected))
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
	lite := s.buildNodeViewsLite()
	for _, m := range nodeMetrics {
		help(m.name, m.help, m.typ)
		for _, v := range lite {
			gauge(m.name, []string{"id=" + q(v.ID), "name=" + q(v.Name)}, m.get(v))
		}
	}

	// 每个状态都输出 1/0，而不是只输出当前那一条。
	// 只输出当前状态时，节点一旦切换状态，旧序列是「消失」而不是归 0 ——
	// Prometheus 里这意味着无法用 rate/absent 判断状态迁移，
	// 告警规则也只能看到「此刻是什么」，看不到「刚才是别的」。
	states := []string{
		model.NodeStateOnline, model.NodeStateStale,
		model.NodeStateOffline, model.NodeStateNoData,
	}
	help("rollcall_node_state", "Node display state (1 for the current state, 0 otherwise).", "gauge")
	for _, v := range lite {
		for _, st := range states {
			val := 0.0
			if v.State == st {
				val = 1
			}
			gauge("rollcall_node_state", []string{"id=" + q(v.ID), "name=" + q(v.Name), "state=" + q(st)}, val)
		}
	}

	// 历史分层：每节点保留的桶数，用于观察降采样是否按预期累积
	help("rollcall_history_points", "History buckets retained per node and resolution.", "gauge")
	for _, n := range s.st.Nodes() {
		counts := s.st.HistoryCounts(n.ID)
		for _, res := range []string{store.HistoryResolution1m, store.HistoryResolution5m, store.HistoryResolution1h} {
			gauge("rollcall_history_points",
				[]string{"id=" + q(n.ID), "name=" + q(n.Name), "resolution=" + q(res)}, float64(counts[res]))
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
		get        func(ok, has bool, lat, cert int, up float64) float64
	}{
		{"rollcall_service_up", "1 if last probe succeeded (NaN = never probed).", false, func(ok, has bool, lat, cert int, up float64) float64 {
			if !has {
				// 从未探测过：输出 NaN 而不是 0，
				// 否则与「探测失败」无法区分，会误触发告警。
				return math.NaN()
			}
			return b2f(ok)
		}},
		// 与 rollcall_service_up 一致：从未探测过输出 NaN。
		// 原来输出 0 ms / 0 天，看起来像"探测成功且延迟 0"，会让面板告警误判。
		{"rollcall_service_latency_milliseconds", "Last probe latency in ms (NaN = never probed).", false, func(ok, has bool, lat, cert int, up float64) float64 {
			if !has {
				return math.NaN()
			}
			return float64(lat)
		}},
		{"rollcall_service_certificate_days_remaining", "TLS cert days left (-1 = n/a, NaN = never probed).", false, func(ok, has bool, lat, cert int, up float64) float64 {
			if !has {
				return math.NaN()
			}
			return float64(cert)
		}},
		{"rollcall_service_uptime_percent_24h", "24h availability percent.", true, func(ok, has bool, lat, cert int, up float64) float64 { return up }},
	}
	for _, m := range svcMetrics {
		help(m.name, m.help, "gauge")
		for _, d := range svcRows {
			// 没有任何 24h 内样本时跳过该 gauge，避免输出误导性的 0%
			if m.needUptime && !d.upOK {
				continue
			}
			gauge(m.name, []string{"id=" + q(d.sv.ID), "name=" + q(d.sv.Name)}, m.get(d.ok, d.has, d.lat, d.cert, d.up))
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
func safeFileName(s string) string {
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

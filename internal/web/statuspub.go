package web

import (
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/motao/rollcall/internal/model"
	"github.com/motao/rollcall/internal/store"
)

// 本文件实现公开状态页（/api/status 家族）的详情与汇总接口。
//
// ★ 安全边界：这里的每一个字段都会被匿名访客看到。
// 任何新增字段都必须先过 cfg.Visibility 的对应开关，
// 默认值以 DefaultStatusPage() 为准（主机指纹类字段默认关闭）。

// statusSummary 是公开页顶部的汇总条。
type statusSummary struct {
	Total       int     `json:"total"`
	Online      int     `json:"online"`
	Stale       int     `json:"stale"`
	Offline     int     `json:"offline"`
	NoData      int     `json:"noData"`
	AvgUptime24 float64 `json:"avgUptime24"` // 有数据的节点 24h 可用率均值
	AvgCPU      float64 `json:"avgCpu"`
	AvgMem      float64 `json:"avgMem"`
	AvgDisk     float64 `json:"avgDisk"`
	TrafficIn   uint64  `json:"trafficIn"`  // 全网实时下行 B/s
	TrafficOut  uint64  `json:"trafficOut"` // 全网实时上行 B/s
	ActiveAlert int     `json:"activeAlert"`
	SvcTotal    int     `json:"svcTotal"`
	SvcDown     int     `json:"svcDown"`
	Maintenance bool    `json:"maintenance"` // 是否存在生效中的维护窗口（全局或任一节点）
}

// statusNodePub 是公开列表里的一个节点行。
type statusNodePub struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Group    string        `json:"group"`
	Online   bool          `json:"online"`
	State    string        `json:"state"`
	CPU      float64       `json:"cpu"`
	Mem      float64       `json:"mem"`
	Disk     float64       `json:"disk"`
	Up       uint64        `json:"uptime"`
	Uptime24 float64       `json:"uptime24"`
	Bars     []statusBar   `json:"bars"`
	Spark    []model.Point `json:"spark,omitempty"` // 24h CPU 迷你曲线（降采样）
	Load1    float64       `json:"load1,omitempty"`
	NetRx    uint64        `json:"netRx,omitempty"` // B/s
	NetTx    uint64        `json:"netTx,omitempty"` // B/s
	Region   string        `json:"region,omitempty"`
	Provider string        `json:"provider,omitempty"`
	// MaintenanceTitle 非空表示该节点正在计划维护中。
	// 前端据此把状态点换成"维护中"并抑制"离线"措辞。
	MaintenanceTitle string `json:"maintenanceTitle,omitempty"`
}

// dailyUptime 是一天的可用率。
// Has 为 false 表示这一天没有任何数据（节点尚未接入），
// 前端应显示为空白格而不是 0% —— 否则新节点会凭空多出一排红色。
type dailyUptime struct {
	TS  int64   `json:"ts"`
	Up  float64 `json:"up"`
	Has bool    `json:"has"`
}

// statusIncident 是事件时间线里的一条。
// 自动事件由告警历史生成，人工事件由管理员在后台登记。
type statusIncident struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Body      string   `json:"body,omitempty"`
	Level     string   `json:"level"`
	NodeNames []string `json:"nodeNames,omitempty"`
	StartAt   int64    `json:"startAt"`
	EndAt     int64    `json:"endAt"` // 0 = 进行中
	Duration  int64    `json:"duration"`
	Auto      bool     `json:"auto"` // true=告警自动生成，false=人工登记
}

// ---------- 汇总 ----------

// buildStatusSummary 汇总公开页顶部指标。
// 可用率只统计"有数据"的节点：把尚未接入的节点算成 0% 会让总览失真。
func (s *Server) buildStatusSummary(nodes []statusNodePub, svcTotal, svcDown int, maint bool) statusSummary {
	sum := statusSummary{
		Total: len(nodes), SvcTotal: svcTotal, SvcDown: svcDown, Maintenance: maint,
		ActiveAlert: s.st.ActiveAlertCount(),
	}
	var upSum, cpuSum, memSum, diskSum float64
	n := 0
	for _, p := range nodes {
		switch p.State {
		case model.NodeStateOnline:
			sum.Online++
		case model.NodeStateStale:
			sum.Stale++
		case model.NodeStateOffline:
			sum.Offline++
		case model.NodeStateNoData:
			sum.NoData++
		}
		if p.State == model.NodeStateNoData {
			continue
		}
		upSum += p.Uptime24
		cpuSum += p.CPU
		memSum += p.Mem
		diskSum += p.Disk
		sum.TrafficIn += p.NetRx
		sum.TrafficOut += p.NetTx
		n++
	}
	if n > 0 {
		sum.AvgUptime24 = round1(upSum / float64(n))
		sum.AvgCPU = round1(cpuSum / float64(n))
		sum.AvgMem = round1(memSum / float64(n))
		sum.AvgDisk = round1(diskSum / float64(n))
	}
	return sum
}

// ---------- 可用率日柱 ----------

// dayStartUTC 返回 ts 所在 UTC 日的起点。
// 服务端统一按 UTC 切日：本地时区切日会让同一份数据在
// 不同时区的部署上得出不同的"每天可用率"，跨时区运维时无法对齐。
func dayStartUTC(ts int64) int64 {
	const day = int64(24 * time.Hour / time.Second)
	return (ts / day) * day
}

// nodeDailyUptime 计算节点最近 days 天、每天的可用率。
//
// 数据源是 1 小时聚合桶：桶内 Count>0 表示该小时至少上报过一次，
// 当天可用率 = 有数据的小时数 / 24。
// 用"小时是否有上报"而不是"桶内平均在线时长"，是因为聚合桶
// 不保留"这一小时里在线了多久"，只保留平均值与峰值。
func (s *Server) nodeDailyUptime(nodeID string, days int, now int64) []dailyUptime {
	if days <= 0 {
		days = 90
	}
	start := dayStartUTC(now-int64(days-1)*86400) - 0
	pts := s.st.HistorySeries(nodeID, store.HistoryResolution1h, start)

	// 按 UTC 日分桶：桶的 TS 就是该小时起点（Index），可直接整除归日。
	perDay := make(map[int64][2]int) // dayStart -> [有数据的小时数, 总小时数]
	for _, p := range pts {
		if p.TS < start || p.Count == 0 {
			continue
		}
		d := dayStartUTC(p.TS)
		v := perDay[d]
		v[1]++
		if p.Count > 0 {
			v[0]++
		}
		perDay[d] = v
	}

	out := make([]dailyUptime, 0, days)
	for i := 0; i < days; i++ {
		d := start + int64(i)*86400
		v, ok := perDay[d]
		item := dailyUptime{TS: d}
		if ok && v[1] > 0 {
			item.Has = true
			item.Up = round1(float64(v[0]) * 100 / float64(v[1]))
		}
		out = append(out, item)
	}
	return out
}

// fleetDailyUptime 计算整站每日可用率：各节点当天可用率的算术平均。
// 只统计当天有数据的节点；全机群当天都无数据时 Has=false。
func (s *Server) fleetDailyUptime(nodeIDs []string, days int, now int64) []dailyUptime {
	per := make([][]dailyUptime, 0, len(nodeIDs))
	for _, id := range nodeIDs {
		per = append(per, s.nodeDailyUptime(id, days, now))
	}
	out := make([]dailyUptime, days)
	for i := 0; i < days; i++ {
		out[i].TS = dayStartUTC(now - int64(days-1-i)*86400)
		var sum float64
		n := 0
		for _, series := range per {
			if i >= len(series) || !series[i].Has {
				continue
			}
			sum += series[i].Up
			n++
		}
		if n > 0 {
			out[i].Has = true
			out[i].Up = round1(sum / float64(n))
		}
	}
	return out
}

// ---------- 事件时间线 ----------

// buildTimeline 合并"告警自动生成的事件"与"人工登记的事件"。
//
// 告警是按规则×节点产生的，同一场故障会在多台机器上各留一条，
// 直接铺开会让时间线被同一件事刷满。这里按「规则 + 分钟级时间窗」归并：
// 同一规则、开始时间相差在 mergeWindow 内的告警合并成一条，
// 节点名以列表形式保留。
func (s *Server) buildTimeline(since int64, limit int, nameOf func(string) string) []statusIncident {
	const mergeWindow int64 = 600 // 10 分钟
	out := make([]statusIncident, 0, limit)

	// ★ 归并结果必须保存在指针里、最后统一取值。
	// 早期写法是 out = append(out, b.inc)（值拷贝）：后续往 b.inc 追加节点名、
	// 修正结束时间都不会反映到已入列的副本上，时间线于是永远只显示
	// 「第一个节点」和「第一次看到的结束时间」。
	type bucket struct {
		inc  statusIncident
		seen map[string]struct{}
	}
	groups := make(map[string]*bucket) // ruleID|窗口序号 -> 归并桶
	var order []*bucket                // 记录首次出现顺序，输出前再统一排序

	now := time.Now().Unix()
	for _, a := range s.st.Alerts(0) {
		if a == nil || a.StartedAt < since {
			continue
		}
		end := a.ResolvedAt
		key := a.RuleID + "|" + itoa64(a.StartedAt/mergeWindow)
		b, ok := groups[key]
		if !ok {
			b = &bucket{
				inc: statusIncident{
					ID:      "a_" + a.ID,
					Title:   a.RuleName,
					Level:   a.Severity,
					StartAt: a.StartedAt,
					EndAt:   end,
					Auto:    true,
				},
				seen: map[string]struct{}{},
			}
			groups[key] = b
			order = append(order, b)
		}
		if _, dup := b.seen[a.NodeID]; !dup {
			b.seen[a.NodeID] = struct{}{}
			if nm := nameOf(a.NodeID); nm != "" {
				b.inc.NodeNames = append(b.inc.NodeNames, nm)
			}
		}
		// 同一窗口内取最早开始；任一分片未恢复则整条事件仍视为进行中
		if a.StartedAt < b.inc.StartAt {
			b.inc.StartAt = a.StartedAt
		}
		if end == 0 {
			b.inc.EndAt = 0
		} else if b.inc.EndAt != 0 && end > b.inc.EndAt {
			b.inc.EndAt = end
		}
	}
	for _, b := range order {
		if b.inc.EndAt == 0 {
			b.inc.Duration = now - b.inc.StartAt
		} else {
			b.inc.Duration = b.inc.EndAt - b.inc.StartAt
		}
		out = append(out, b.inc)
	}

	for _, it := range s.st.Incidents() {
		if it == nil || it.StartAt < since {
			continue
		}
		names := make([]string, 0, len(it.NodeIDs))
		for _, id := range it.NodeIDs {
			if nm := nameOf(id); nm != "" {
				names = append(names, nm)
			}
		}
		out = append(out, statusIncident{
			ID:        "i_" + it.ID,
			Title:     it.Title,
			Body:      it.Body,
			Level:     it.Level,
			NodeNames: names,
			StartAt:   it.StartAt,
			EndAt:     it.EndAt,
			Duration:  endOr(it.EndAt, now) - it.StartAt,
			Auto:      false,
		})
	}

	// 最近的在前
	sort.Slice(out, func(i, j int) bool { return out[i].StartAt > out[j].StartAt })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func endOr(v, def int64) int64 {
	if v == 0 {
		return def
	}
	return v
}

// itoa64 是 strconv.FormatInt 的轻量别名，避免在热路径上重复写包名。
func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [24]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// ---------- 实时速率 ----------

// netRates 由累计字节数算实时速率（B/s）。
//
// 只用最后两个采样点：更长的窗口会把短时突发抹平，
// 而状态页要看的正是"现在有多快"。
// 计数器回绕（网卡重置、系统重装）时返回 0，不产生天文数字。
func netRates(series []model.Metrics) (rx, tx uint64) {
	if len(series) < 2 {
		return 0, 0
	}
	last := series[len(series)-1]
	prev := series[len(series)-2]
	dt := last.TS - prev.TS
	if dt <= 0 {
		return 0, 0
	}
	drx := last.NetRx - prev.NetRx
	dtx := last.NetTx - prev.NetTx
	// 无符号减法回绕会得到巨大值，按回绕处理
	if last.NetRx < prev.NetRx {
		drx = 0
	}
	if last.NetTx < prev.NetTx {
		dtx = 0
	}
	if drx > math.MaxUint64/uint64(dt) {
		drx = 0
	}
	if dtx > math.MaxUint64/uint64(dt) {
		dtx = 0
	}
	return drx / uint64(dt), dtx / uint64(dt)
}

// ---------- 节点详情（公开） ----------

// handleStatusNode 返回单个节点在公开页上的详情。
// 字段按 cfg.Visibility 裁剪：未开启的字段一律省略，
// 而不是下发空字符串 —— 空字符串会让前端误判为"有值但为空"。
func (s *Server) handleStatusNode(w http.ResponseWriter, r *http.Request) {
	cfg := s.st.StatusPage()
	if !cfg.Blocks.NodeDetail {
		fail(w, 404, "not found")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/status/node/")
	id := rest
	if i := strings.IndexByte(id, '/'); i >= 0 {
		id = id[:i]
	}
	if id == "" {
		fail(w, 400, "missing node id")
		return
	}
	n, ok := s.st.Node(id)
	if !ok {
		fail(w, 404, "node not found")
		return
	}

	now := time.Now().Unix()
	m, ts, _ := s.st.Latest(id)
	state := store.NodeState(ts, now)
	_, maintNodes := s.st.MaintenanceAt(now)
	vis := cfg.Visibility

	type latestOut struct {
		CPU       float64 `json:"cpu,omitempty"`
		MemPct    float64 `json:"memPct,omitempty"`
		DiskPct   float64 `json:"diskPct,omitempty"`
		Load1     float64 `json:"load1,omitempty"`
		Load5     float64 `json:"load5,omitempty"`
		Load15    float64 `json:"load15,omitempty"`
		Uptime    uint64  `json:"uptime,omitempty"`
		Procs     int     `json:"procs,omitempty"`
		TCPConn   int     `json:"tcpConn,omitempty"`
		NetRx     uint64  `json:"netRx,omitempty"` // B/s
		NetTx     uint64  `json:"netTx,omitempty"` // B/s
		TotRx     uint64  `json:"totRx,omitempty"`
		TotTx     uint64  `json:"totTx,omitempty"`
		MemUsed   uint64  `json:"memUsed,omitempty"`
		MemTotal  uint64  `json:"memTotal,omitempty"`
		DiskUsed  uint64  `json:"diskUsed,omitempty"`
		DiskTotal uint64  `json:"diskTotal,omitempty"`
		SwapUsed  uint64  `json:"swapUsed,omitempty"`
		SwapTotal uint64  `json:"swapTotal,omitempty"`
		Cores     int     `json:"cores,omitempty"`
	}
	var lo latestOut
	if vis.Metrics {
		lo.CPU = round1(m.CPUPct)
		lo.MemPct = round1(pctF(m.MemUsed, m.MemTotal))
		lo.DiskPct = round1(pctF(m.DiskUsed, m.DiskTotal))
		lo.MemUsed, lo.MemTotal = m.MemUsed, m.MemTotal
		lo.DiskUsed, lo.DiskTotal = m.DiskUsed, m.DiskTotal
	}
	if vis.Load {
		lo.Load1, lo.Load5, lo.Load15 = round2(m.Load1), round2(m.Load5), round2(m.Load15)
	}
	if vis.Uptime {
		lo.Uptime = m.Uptime
	}
	if vis.Procs {
		lo.Procs = m.Procs
	}
	if vis.TCPConn {
		lo.TCPConn = m.TCPConn
	}
	if vis.Swap {
		lo.SwapUsed, lo.SwapTotal = m.SwapUsed, m.SwapTotal
	}
	if vis.Traffic {
		series := s.st.Series(id, now-1800)
		lo.NetRx, lo.NetTx = netRates(series)
		lo.TotRx, lo.TotTx = m.NetRx, m.NetTx
	}
	if vis.Cores {
		if h := s.st.Host(id); h.Cores > 0 {
			lo.Cores = h.Cores
		}
	}

	// 主机信息：每一项各自受开关控制
	host := map[string]any{}
	h := s.st.Host(id)
	if vis.Hostname && h.Hostname != "" {
		host["hostname"] = h.Hostname
	}
	if vis.OS && h.OS != "" {
		host["os"] = h.OS
	}
	if vis.Kernel && h.Kernel != "" {
		host["kernel"] = h.Kernel
	}
	if vis.Arch && h.Arch != "" {
		host["arch"] = h.Arch
	}
	if vis.CPUModel && h.CPUModel != "" {
		host["cpuModel"] = h.CPUModel
	}
	if vis.Cores && h.Cores > 0 {
		host["cores"] = h.Cores
	}
	if vis.Virt && h.Virt != "" {
		host["virt"] = h.Virt
	}

	// 24h 曲线：从原始序列取，最多降采样到 ~120 点，
	// 再多前端也画不出来，只会拖慢首屏。
	series := s.st.Series(id, now-86400)
	step := 1
	if len(series) > 120 {
		step = len(series) / 120
	}
	cpu, mem, disk, nrx, ntx := make([]model.Point, 0, 128), make([]model.Point, 0, 128), make([]model.Point, 0, 128), make([]model.Point, 0, 128), make([]model.Point, 0, 128)
	for i := 0; i < len(series); i += step {
		p := series[i]
		if vis.Metrics {
			cpu = append(cpu, model.Point{T: p.TS, V: round1(p.CPUPct)})
			mem = append(mem, model.Point{T: p.TS, V: round1(pctF(p.MemUsed, p.MemTotal))})
			disk = append(disk, model.Point{T: p.TS, V: round1(pctF(p.DiskUsed, p.DiskTotal))})
		}
		if vis.Traffic && i > 0 {
			prev := series[i-1]
			dt := p.TS - prev.TS
			if dt > 0 && p.NetRx >= prev.NetRx {
				nrx = append(nrx, model.Point{T: p.TS, V: float64((p.NetRx - prev.NetRx) / uint64(dt))})
			}
			if dt > 0 && p.NetTx >= prev.NetTx {
				ntx = append(ntx, model.Point{T: p.TS, V: float64((p.NetTx - prev.NetTx) / uint64(dt))})
			}
		}
	}

	// 该节点名下的服务探测
	svcs := make([]map[string]any, 0)
	for _, sv := range s.st.Services() {
		if sv.NodeID != id {
			continue
		}
		_, lat, ok, has := s.st.ServiceLast(sv.ID)
		rate, _ := serviceUptime24(s.st.ServiceSeries(sv.ID), now)
		item := map[string]any{"name": sv.Name, "has": has, "uptime": round1(rate)}
		if has {
			item["ok"] = ok
			item["latencyMs"] = lat
		}
		svcs = append(svcs, item)
	}

	node := map[string]any{
		"id":    n.ID,
		"name":  n.Name,
		"group": store.VisibleGroup(n.Group),
	}
	if vis.Region && n.Region != "" {
		node["region"] = n.Region
	}
	if vis.Provider && n.Provider != "" {
		node["provider"] = n.Provider
	}

	uptime24, bars := uptime24(series, now, state == model.NodeStateOnline)
	mTitle := ""
	if m, ok := maintNodes[id]; ok && m != nil {
		mTitle = m.Title
	}

	writeJSON(w, 200, map[string]any{
		"node":        node,
		"host":        host,
		"latest":      lo,
		"state":       state,
		"online":      state == model.NodeStateOnline,
		"lastSeen":    ts,
		"uptime24":    round1(uptime24),
		"bars":        bars,
		"maintenance": mTitle,
		"series":      map[string]any{"cpu": cpu, "mem": mem, "disk": disk, "netRx": nrx, "netTx": ntx},
		"daily":       s.nodeDailyUptime(id, cfg.HistoryDays, now),
		"services":    svcs,
		"generatedAt": now,
		"visibility":  vis,
	})
}

func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }

// handleStatusIncidents 单独暴露事件时间线，便于详情页/前端按需加载。
func (s *Server) handleStatusIncidents(w http.ResponseWriter, r *http.Request) {
	cfg := s.st.StatusPage()
	if !cfg.Blocks.Incidents {
		writeJSON(w, 200, map[string]any{"items": []statusIncident{}})
		return
	}
	days := cfg.HistoryDays
	now := time.Now().Unix()
	names := map[string]string{}
	for _, n := range s.st.Nodes() {
		names[n.ID] = n.Name
	}
	writeJSON(w, 200, map[string]any{
		"items":       s.buildTimeline(now-int64(days)*86400, 200, func(id string) string { return names[id] }),
		"generatedAt": now,
	})
}

// statusHandler 公开状态页自身的 HTTP 方法约束。
// 状态页只读，任何写方法都没有意义，显式拒绝比静默忽略更利于排障。
func rejectNonGET(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			fail(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		next(w, r)
	}
}

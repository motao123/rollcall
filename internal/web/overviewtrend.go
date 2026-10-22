package web

import (
	"net/http"
	"sort"
	"time"

	"github.com/motao123/rollcall/internal/store"
)

// trendPoint 是机群级聚合的一个时间桶。
// CPU/内存/磁盘是"有上报节点的平均值"，网速是全部节点之和（B/s）。
type trendPoint struct {
	T      int64   `json:"t"`
	CPU    float64 `json:"cpu"`
	Mem    float64 `json:"mem"`
	Disk   float64 `json:"disk"`
	NetIn  float64 `json:"netIn"`
	NetOut float64 `json:"netOut"`
	Nodes  int     `json:"nodes"`
}

type topNode struct {
	ID    string  `json:"id"`
	Name  string  `json:"name"`
	State string  `json:"state"`
	Value float64 `json:"value"`
}

// handleOverviewTrend 返回机群趋势与资源 Top N。
//
//	GET /api/overview/trend?range=24h|7d
//
// 24h 用 5m 桶（288 点），7d 用 1h 桶（168 点）：
// 直接用 1m 原始点会把 24 小时变成 1440 点 × N 台，
// 前端画不下，传输也没必要。
func (s *Server) handleOverviewTrend(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	var window int64
	res := store.HistoryResolution5m
	switch r.URL.Query().Get("range") {
	case "", "24h":
		window = 86400
	case "7d":
		window = 7 * 86400
		res = store.HistoryResolution1h
	default:
		fail(w, http.StatusBadRequest, "未知的时间范围")
		return
	}
	since := now - window
	interval := 300.0
	if res == store.HistoryResolution1h {
		interval = 3600
	}

	type agg struct {
		cpu, mem, disk, netin, netout float64
		n                             int
	}
	buckets := map[int64]*agg{}
	for _, n := range s.st.Nodes() {
		for _, p := range s.st.HistorySeries(n.ID, res, since) {
			if p.Count == 0 {
				continue
			}
			a := buckets[p.TS]
			if a == nil {
				a = &agg{}
				buckets[p.TS] = a
			}
			a.cpu += p.CPUPct
			a.mem += p.MemPct
			a.disk += p.DiskPct
			a.netin += float64(p.NetRxDelta)
			a.netout += float64(p.NetTxDelta)
			a.n++
		}
	}
	keys := make([]int64, 0, len(buckets))
	for ts := range buckets {
		keys = append(keys, ts)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	points := make([]trendPoint, 0, len(keys))
	for _, ts := range keys {
		a := buckets[ts]
		points = append(points, trendPoint{
			T:      ts,
			CPU:    round1(a.cpu / float64(a.n)),
			Mem:    round1(a.mem / float64(a.n)),
			Disk:   round1(a.disk / float64(a.n)),
			NetIn:  round1(a.netin / interval),
			NetOut: round1(a.netout / interval),
			Nodes:  a.n,
		})
	}

	top := map[string][]topNode{"cpu": {}, "mem": {}, "disk": {}, "net": {}}
	for _, v := range s.buildNodeViewsLite() {
		mem := pctF(v.Latest.MemUsed, v.Latest.MemTotal)
		disk := pctF(v.Latest.DiskUsed, v.Latest.DiskTotal)
		base := topNode{ID: v.ID, Name: v.Name, State: v.State}
		addTop := func(key string, val float64) {
			t := base
			t.Value = round1(val)
			top[key] = append(top[key], t)
		}
		addTop("cpu", v.Latest.CPUPct)
		addTop("mem", mem)
		addTop("disk", disk)
		addTop("net", s.nodeNetRate(v.ID, now))
	}
	for k, list := range top {
		sort.Slice(list, func(i, j int) bool {
			if list[i].Value != list[j].Value {
				return list[i].Value > list[j].Value
			}
			return list[i].Name < list[j].Name
		})
		if len(list) > 8 {
			list = list[:8]
		}
		top[k] = list
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"range":       r.URL.Query().Get("range"),
		"resolution":  res,
		"points":      points,
		"top":         top,
		"generatedAt": now,
	})
}

// nodeNetRate 从最近两个原始点估算当前网速（B/s）。
// 计数器回绕或样本过旧时返回 0，而不是给出负值或一个离谱的峰值。
func (s *Server) nodeNetRate(id string, now int64) float64 {
	pts := s.st.Series(id, now-900)
	if len(pts) < 2 {
		return 0
	}
	last, prev := pts[len(pts)-1], pts[len(pts)-2]
	dt := last.TS - prev.TS
	if dt <= 0 || now-last.TS > 300 {
		return 0
	}
	var rx, tx uint64
	if last.NetRx >= prev.NetRx {
		rx = last.NetRx - prev.NetRx
	}
	if last.NetTx >= prev.NetTx {
		tx = last.NetTx - prev.NetTx
	}
	return float64(rx+tx) / float64(dt)
}

package web

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/store"
)

// compareNode 是多节点对比的单个数据行。
type compareNode struct {
	ID       string             `json:"id"`
	Name     string             `json:"name"`
	Group    string             `json:"group"`
	Region   string             `json:"region"`
	Country  string             `json:"country,omitempty"`
	Favorite bool               `json:"favorite"`
	State    string             `json:"state"`
	CPUPct   float64            `json:"cpuPct"`
	MemPct   float64            `json:"memPct"`
	DiskPct  float64            `json:"diskPct"`
	Load1    float64            `json:"load1"`
	NetRx    uint64             `json:"netRx"`
	NetTx    uint64             `json:"netTx"`
	Uptime   uint64             `json:"uptime"`
	Traffic  model.TrafficUsage `json:"traffic"`
	Price    float64            `json:"price"`
	Currency string             `json:"currency"`
	ExpireAt string             `json:"expireAt"`
	LastSeen int64              `json:"lastSeen"`
}

type groupAgg struct {
	Group     string  `json:"group"`
	Nodes     int     `json:"nodes"`
	Online    int     `json:"online"`
	AvgCPU    float64 `json:"avgCpu"`
	AvgMem    float64 `json:"avgMem"`
	Cost      float64 `json:"cost"`
	Remaining float64 `json:"remaining"`
	TrafficIn uint64  `json:"trafficIn"`
}

type compareSummary struct {
	Nodes       int        `json:"nodes"`
	Online      int        `json:"online"`
	AvgCPU      float64    `json:"avgCpu"`
	AvgMem      float64    `json:"avgMem"`
	MaxCPUPct   float64    `json:"maxCpu"`
	MaxMemPct   float64    `json:"maxMem"`
	Cost        float64    `json:"cost"`
	Remaining   float64    `json:"remaining"`
	TrafficIn   uint64     `json:"trafficIn"`
	Groups      []groupAgg `json:"groups"`
	Favorites   int        `json:"favorites"`
	Unavailable int        `json:"unavailable"`
}

// handleCompare 返回跨节点对比与按分组聚合。
// 支持 ?nodeIds=a,b 限定范围、?group=xxx 过滤分组、?favorite=1 只看收藏。
func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	q := r.URL.Query()
	idFilter := map[string]bool{}
	for _, id := range strings.Split(q.Get("nodeIds"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			idFilter[id] = true
		}
	}
	groupFilter := strings.TrimSpace(q.Get("group"))
	onlyFav := q.Get("favorite") == "1" || q.Get("favorite") == "true"

	now := time.Now().Unix()
	rows := make([]compareNode, 0)
	grouped := map[string]*groupAgg{}
	var sumCPU, sumMem float64
	var onlineCnt, favCnt, unavailable int

	for _, n := range s.st.Nodes() {
		if len(idFilter) > 0 && !idFilter[n.ID] {
			continue
		}
		if groupFilter != "" && n.Group != groupFilter {
			continue
		}
		if onlyFav && !n.Favorite {
			continue
		}
		m, ts, _ := s.st.Latest(n.ID)
		state := store.NodeState(ts, now)
		// 最新快照里的 NetRx/NetTx 是累计计数，不是实时速率；
		// 对比页要展示 B/s，必须像公开状态页一样用最近两点求差。
		rx, tx := netRates(s.st.Series(n.ID, now-1800))
		row := compareNode{
			ID: n.ID, Name: n.Name, Group: n.Group, Region: n.Region,
			Country: n.Country, Favorite: n.Favorite, State: state,
			CPUPct: m.CPUPct, MemPct: pctF(m.MemUsed, m.MemTotal),
			DiskPct: pctF(m.DiskUsed, m.DiskTotal), Load1: m.Load1,
			NetRx: rx, NetTx: tx, Uptime: m.Uptime,
			Traffic: s.st.TrafficUsage(n.ID),
			Price:   n.Price, Currency: n.Currency, ExpireAt: n.ExpireAt,
			LastSeen: ts,
		}
		rows = append(rows, row)

		// 查与写必须用同一个 key：原来查 grouped[n.Group]（空分组是 ""）、
		// 写 grouped["未分组"]，导致每台未分组节点都新建一个聚合并覆盖前一个，
		// 最后只剩最后一台的统计。
		label := n.Group
		if label == "" {
			label = "未分组"
		}
		g := grouped[label]
		if g == nil {
			g = &groupAgg{Group: label}
			grouped[label] = g
		}
		g.Nodes++
		g.Cost += monthly(n.Price, n.Currency, s.cnyRate)
		g.Remaining += remainingValue(n.Price, n.Currency, s.cnyRate, n.ExpireAt)
		if state == model.NodeStateOnline {
			onlineCnt++
			sumCPU += m.CPUPct
			sumMem += pctF(m.MemUsed, m.MemTotal)
			g.Online++
			g.AvgCPU += m.CPUPct
			g.AvgMem += pctF(m.MemUsed, m.MemTotal)
			g.TrafficIn += rx
		} else {
			unavailable++
		}
		if n.Favorite {
			favCnt++
		}
	}

	// 收藏节点优先，其次按名称排序，便于在对比表里快速找到常用机器。
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Favorite != rows[j].Favorite {
			return rows[i].Favorite
		}
		return strings.ToLower(rows[i].Name) < strings.ToLower(rows[j].Name)
	})

	sum := compareSummary{
		Nodes: len(rows), Online: onlineCnt, Favorites: favCnt,
		Unavailable: unavailable, Groups: make([]groupAgg, 0, len(grouped)),
	}
	if onlineCnt > 0 {
		sum.AvgCPU = sumCPU / float64(onlineCnt)
		sum.AvgMem = sumMem / float64(onlineCnt)
	}
	for _, r := range rows {
		if r.State != model.NodeStateOnline {
			continue
		}
		if r.CPUPct > sum.MaxCPUPct {
			sum.MaxCPUPct = r.CPUPct
		}
		if r.MemPct > sum.MaxMemPct {
			sum.MaxMemPct = r.MemPct
		}
		// 只累加"实时资源"类指标：成本不在这里算（见下）。
		sum.TrafficIn += r.NetRx
	}
	// ★ 成本口径必须与分组一致：分组对**全部节点**累加（成本是资产属性，
	// 机器离线也照样付钱），这里若只算在线节点，页面上"分组成本之和"
	// 就会大于"合计成本"，同一页两个数对不上。剩余价值同理。
	for _, r := range rows {
		sum.Cost += monthly(r.Price, r.Currency, s.cnyRate)
		sum.Remaining += remainingValue(r.Price, r.Currency, s.cnyRate, r.ExpireAt)
	}
	for _, g := range grouped {
		if g.Online > 0 {
			g.AvgCPU /= float64(g.Online)
			g.AvgMem /= float64(g.Online)
		}
		sum.Groups = append(sum.Groups, *g)
	}
	sort.Slice(sum.Groups, func(i, j int) bool { return sum.Groups[i].Group < sum.Groups[j].Group })

	writeJSON(w, http.StatusOK, map[string]any{"nodes": rows, "summary": sum})
}

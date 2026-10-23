package web

import (
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/motao123/rollcall/internal/model"
)

// 服务探测统一由控制服务器发起，NodeID 只是展示归组。
// 同目标视图必须把这一点写进响应，避免被误读成"各节点本地视角"。
const serviceCompareNote = "所有探测均由控制服务器发起；“归属节点”只用于分组展示，探测并不在该节点上执行。"

type serviceCompareRow struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Type      string  `json:"type"`
	Target    string  `json:"target"`
	NodeID    string  `json:"nodeId"`
	NodeName  string  `json:"nodeName"`
	Enabled   bool    `json:"enabled"`
	Has       bool    `json:"has"`
	OK        bool    `json:"ok"`
	LatencyMs int     `json:"latencyMs"`
	CertDays  int     `json:"certDays"`
	LossPct   float64 `json:"lossPct"`
	JitterMs  float64 `json:"jitterMs"`
	Uptime    float64 `json:"uptime"`
	LastTS    int64   `json:"lastTs"`
}

type serviceCompareGroup struct {
	Host     string              `json:"host"`
	Verdict  string              `json:"verdict"` // up | partial | down | unknown | disabled
	Total    int                 `json:"total"`
	Enabled  int                 `json:"enabled"`
	Probed   int                 `json:"probed"`
	Failing  int                 `json:"failing"`
	Services []serviceCompareRow `json:"services"`
}

type serviceCompareResponse struct {
	Vantage   string                `json:"vantage"`
	Note      string                `json:"note"`
	Groups    []serviceCompareGroup `json:"groups"`
	Singles   int                   `json:"singles"`
	Ungrouped int                   `json:"ungrouped"`
}

// handleServiceCompare 按主机聚合探测目标，返回同目标并排数据。
// 只有包含至少两条探测的主机才会进入 groups；单条目标只计数，
// 避免把普通服务列表原样重复一遍。
func (s *Server) handleServiceCompare(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	nodeNames := make(map[string]string)
	for _, n := range s.st.Nodes() {
		nodeNames[n.ID] = n.Name
	}

	byHost := make(map[string]*serviceCompareGroup)
	singles, ungrouped := 0, 0
	for _, sv := range s.st.Services() {
		host := probeTargetHost(sv.Type, sv.Target)
		if host == "" {
			ungrouped++
			continue
		}
		g := byHost[host]
		if g == nil {
			g = &serviceCompareGroup{Host: host}
			byHost[host] = g
		}
		g.Services = append(g.Services, s.serviceCompareRow(sv, nodeNames, now))
	}

	groups := make([]serviceCompareGroup, 0, len(byHost))
	for _, g := range byHost {
		if len(g.Services) < 2 {
			singles++
			continue
		}
		// 固定按归属节点、名称排序：并排对比的价值在于行位置稳定，
		// 不能像服务列表那样按异常状态重排，否则每次刷新都换位。
		sort.SliceStable(g.Services, func(i, j int) bool {
			a, b := g.Services[i], g.Services[j]
			if a.Enabled != b.Enabled {
				return a.Enabled
			}
			// 未关联节点的探测排在最后，先展示可定位到具体机器的行。
			if (a.NodeName == "") != (b.NodeName == "") {
				return b.NodeName == ""
			}
			if a.NodeName != b.NodeName {
				return a.NodeName < b.NodeName
			}
			if a.Name != b.Name {
				return a.Name < b.Name
			}
			return a.ID < b.ID
		})
		g.Total = len(g.Services)
		for _, row := range g.Services {
			if !row.Enabled {
				continue
			}
			g.Enabled++
			if !row.Has {
				continue
			}
			g.Probed++
			if !row.OK {
				g.Failing++
			}
		}
		switch {
		case g.Enabled == 0:
			g.Verdict = "disabled"
		case g.Probed == 0:
			g.Verdict = "unknown"
		case g.Failing == 0:
			g.Verdict = "up"
		case g.Failing == g.Probed:
			g.Verdict = "down"
		default:
			g.Verdict = "partial"
		}
		groups = append(groups, *g)
	}

	// 异常组优先，其次按主机名；同一屏内先看到需要处理的目标。
	rank := map[string]int{"down": 0, "partial": 1, "unknown": 2, "disabled": 3, "up": 4}
	sort.Slice(groups, func(i, j int) bool {
		ri, rj := rank[groups[i].Verdict], rank[groups[j].Verdict]
		if ri != rj {
			return ri < rj
		}
		return groups[i].Host < groups[j].Host
	})

	writeJSON(w, http.StatusOK, serviceCompareResponse{
		Vantage: "control-server", Note: serviceCompareNote,
		Groups: groups, Singles: singles, Ungrouped: ungrouped,
	})
}

func (s *Server) serviceCompareRow(sv *model.Service, nodeNames map[string]string, now int64) serviceCompareRow {
	row := serviceCompareRow{
		ID: sv.ID, Name: sv.Name, Type: sv.Type, Target: sv.Target,
		NodeID: sv.NodeID, NodeName: nodeNames[sv.NodeID],
		Enabled: sv.Enabled, CertDays: -1,
	}
	if ts, latency, ok, cert, has := s.st.ServiceLastFull(sv.ID); has {
		row.Has, row.OK, row.LastTS = true, ok, ts
		row.LatencyMs, row.CertDays = latency, cert
	}
	st := s.st.ServiceStats(sv.ID, now-3600)
	row.LossPct, row.JitterMs = st.LossPct, st.JitterMs
	row.Uptime = serviceUptime24h(s.st.ServiceSeries(sv.ID), now)
	return row
}

// serviceUptime24h 计算最近 24 小时可用率。ServiceSeries 返回整个
// 环形缓冲（可能超过 24 小时），必须显式截断。
func serviceUptime24h(hist [][3]int64, now int64) float64 {
	cutoff := now - 86400
	up, total := 0, 0
	for _, h := range hist {
		if h[0] < cutoff {
			continue
		}
		total++
		if h[2] == 1 {
			up++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(up) / float64(total) * 100
}

// probeTargetHost 把不同类型的探测目标归一到同一主机名。
// 例如 https://example.com/health、example.com:443、example.com@1.1.1.1
// 都属于 example.com，可在同一组内并排比较。
func probeTargetHost(typ, target string) string {
	t := strings.TrimSpace(target)
	if t == "" {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(typ)) {
	case "http", "https":
		if u, err := url.Parse(t); err == nil && u.Hostname() != "" {
			return normalizeProbeHost(u.Hostname())
		}
		// 兼容缺少 scheme 的历史配置：url.Parse 会把裸主机当成路径。
		if u, err := url.Parse("//" + t); err == nil && u.Hostname() != "" {
			return normalizeProbeHost(u.Hostname())
		}
	case "tcp":
		if h, _, err := net.SplitHostPort(t); err == nil && h != "" {
			return normalizeProbeHost(h)
		}
	case "dns":
		h := t
		if i := strings.LastIndex(h, "@"); i >= 0 {
			h = strings.TrimSpace(h[:i])
		}
		if h != "" {
			return normalizeProbeHost(h)
		}
	case "icmp":
		// ICMP 目标就是裸主机（不带端口），可直接归一化。
		return normalizeProbeHost(t)
	}
	// 无法解析的目标仍按原文归组，避免从视图中静默消失。
	return normalizeProbeHost(t)
}

func normalizeProbeHost(h string) string {
	h = strings.TrimSpace(strings.ToLower(h))
	h = strings.TrimSuffix(h, ".")
	h = strings.TrimPrefix(h, "[")
	h = strings.TrimSuffix(h, "]")
	if ip := net.ParseIP(h); ip != nil {
		return ip.String()
	}
	return h
}

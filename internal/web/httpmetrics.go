package web

import (
	"sort"
	"sync"
	"time"
)

// HTTP 指标的延迟桶（秒）。覆盖静态资源与 API 的常见区间：
// 命中率低的桶只在 Prometheus 侧多一点序列，不影响进程内存。
var httpLatencyBuckets = [...]float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

// httpMetricsMaxKeys 限制指标维度总数。
// 路由用 ServeMux pattern 而不是真实路径，正常情况下几十个键就封顶；
// 这个上限只防未来新增路由失控，以及异常情况下 pattern 退化为具体路径。
const httpMetricsMaxKeys = 4096

type httpReqKey struct {
	route  string
	method string
	status int
}

type httpDurKey struct {
	route  string
	method string
}

type httpHist struct {
	count   uint64
	sum     float64
	buckets [len(httpLatencyBuckets)]uint64
	inf     uint64
}

// httpMetrics 聚合请求量、状态码与延迟分布。
//
// 节点 ID、令牌 ID 等路径参数一律不进入标签：它们会让时间序列基数
// 随节点/令牌数量线性增长。ServeMux 的 pattern（/api/nodes/）已足够
// 定位"哪个接口慢/在报错"。
type httpMetrics struct {
	mu        sync.Mutex
	requests  map[httpReqKey]uint64
	durations map[httpDurKey]*httpHist
	inflight  int64
}

func newHTTPMetrics() *httpMetrics {
	return &httpMetrics{
		requests:  make(map[httpReqKey]uint64),
		durations: make(map[httpDurKey]*httpHist),
	}
}

func (m *httpMetrics) begin() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.inflight++
	m.mu.Unlock()
}

func (m *httpMetrics) end(route, method string, status int, d time.Duration) {
	if m == nil {
		return
	}
	sec := d.Seconds()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inflight > 0 {
		m.inflight--
	}
	rk := httpReqKey{route: route, method: method, status: status}
	if _, ok := m.requests[rk]; ok || len(m.requests) < httpMetricsMaxKeys {
		m.requests[rk]++
	}
	dk := httpDurKey{route: route, method: method}
	h := m.durations[dk]
	if h == nil {
		if len(m.durations) >= httpMetricsMaxKeys {
			return
		}
		h = &httpHist{}
		m.durations[dk] = h
	}
	h.count++
	h.sum += sec
	for i, le := range httpLatencyBuckets {
		if sec <= le {
			h.buckets[i]++
			return
		}
	}
	h.inf++
}

type httpReqRow struct {
	key httpReqKey
	val uint64
}

type httpDurRow struct {
	key httpDurKey
	val httpHist
}

// httpMetricsSnapshot 是渲染 /metrics 时使用的快照。
type httpMetricsSnapshot struct {
	requests []httpReqRow
	durs     []httpDurRow
	inflight int64
}

func (m *httpMetrics) snapshot() httpMetricsSnapshot {
	if m == nil {
		return httpMetricsSnapshot{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	snap := httpMetricsSnapshot{
		requests: make([]httpReqRow, 0, len(m.requests)),
		durs:     make([]httpDurRow, 0, len(m.durations)),
		inflight: m.inflight,
	}
	for k, v := range m.requests {
		snap.requests = append(snap.requests, httpReqRow{key: k, val: v})
	}
	for k, v := range m.durations {
		snap.durs = append(snap.durs, httpDurRow{key: k, val: *v})
	}
	sort.Slice(snap.requests, func(i, j int) bool {
		a, b := snap.requests[i].key, snap.requests[j].key
		if a.route != b.route {
			return a.route < b.route
		}
		if a.method != b.method {
			return a.method < b.method
		}
		return a.status < b.status
	})
	sort.Slice(snap.durs, func(i, j int) bool {
		a, b := snap.durs[i].key, snap.durs[j].key
		if a.route != b.route {
			return a.route < b.route
		}
		return a.method < b.method
	})
	return snap
}

// isStreamRoute 判断是否为长连接：SSE 的耗时等于连接存活时间，
// 计入延迟直方图会把 p99 永久推到几分钟，失去排障意义。
func isStreamRoute(route string) bool {
	return route == "/api/stream" || route == "/api/status/stream"
}

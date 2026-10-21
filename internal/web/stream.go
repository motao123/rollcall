package web

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

// eventHub 是一个极简的变更广播器，用于 SSE 推送。
//
// 设计取舍：
//   - 只广播"数据变了"这一事实（单调递增的版本号），不广播具体内容。
//     推送内容一旦进入 hub，就要为每个订阅者做一次序列化与缓冲，
//     节点多时内存与 CPU 都会随订阅者数量线性增长；而客户端拿到版本号后
//     拉一次 /api/live 或 /api/overview 即可，逻辑与轮询完全一致。
//   - 订阅者用"容量 1 的信号通道"，满了就丢弃：慢客户端不该拖慢上报路径，
//     下一次变更会把它补上（版本号是绝对值，不是增量）。
//   - 全程不引入外部依赖，只用标准库。
type eventHub struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
	rev  uint64
}

func newEventHub() *eventHub {
	return &eventHub{subs: make(map[chan struct{}]struct{})}
}

// Publish 递增版本号并唤醒所有订阅者。在锁外做非阻塞发送，
// 避免某个卡住的订阅者把上报路径一起拖住。
func (h *eventHub) Publish() {
	h.mu.Lock()
	h.rev++
	rev := h.rev
	subs := make([]chan struct{}, 0, len(h.subs))
	for ch := range h.subs {
		subs = append(subs, ch)
	}
	h.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- struct{}{}:
		default: // 已有未消费的通知，丢弃即可
		}
	}
	_ = rev
}

// Revision 返回当前版本号。
func (h *eventHub) Revision() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.rev
}

// maxStreamSubscribers 限制事件流订阅者总数。公开端点意味着任何匿名请求
// 都能占住一条长连接；不设上限时，少量只建连不读取的客户端就能耗光 goroutine。
const maxStreamSubscribers = 256

// subscribe 在订阅者已满时返回 ok=false，由调用方回 503。
func (h *eventHub) subscribe() (chan struct{}, func(), bool) {
	h.mu.Lock()
	if len(h.subs) >= maxStreamSubscribers {
		h.mu.Unlock()
		return nil, nil, false
	}
	ch := make(chan struct{}, 1)
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}, true
}

// subscriberCount 供 /api/admin/health 观察长连接数量。
func (h *eventHub) subscriberCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// publishChange 在数据发生变化时调用。nil 安全（测试里可能没有初始化 hub）。
func (s *Server) publishChange() {
	if s.events != nil {
		s.events.Publish()
	}
}

// handleStream 是 SSE 端点：服务端主动告诉客户端"数据变了"。
//
// 为什么用 SSE 而不是 WebSocket：
//   - 标准库即可实现，不需要 gorilla/websocket 之类的依赖（本项目零外部依赖）；
//   - 单向推送正好匹配需求（客户端从不通过这条通道发指令）；
//   - 浏览器 EventSource 自带断线重连与 Last-Event-ID，穿透大多数反代；
//   - 只需一个普通的 GET，鉴权复用现有 Cookie 会话。
//
// 事件格式：
//
//	event: hello  data: {"rev":N}     —— 首帧，客户端据此对齐版本
//	event: change data: {"rev":N}     —— 数据变化
//	event: ping   data: {"rev":N}     —— 25 秒心跳（保活，兼作兜底版本对齐）
//
// 同一个 handler 同时服务需鉴权的 /api/stream 与公开的 /api/status/stream；
// 两者都只推版本号，不推数据，差别只在路由表里的鉴权级别。
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	if s.events == nil {
		// 理论上 New() 一定初始化；直接构造 Server 的测试不该 500，给个明确错误。
		fail(w, http.StatusServiceUnavailable, "事件推送未初始化")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, http.StatusInternalServerError, "当前连接不支持流式响应")
		return
	}
	// 先占订阅名额再写响应头：满了还能干净地回 503，而不是先发 hello 再断流。
	ch, cancel, ok := s.events.subscribe()
	if !ok {
		fail(w, http.StatusServiceUnavailable, "实时连接数已满，请稍后重试")
		return
	}
	defer cancel()

	// ★ 必须清掉写超时：http.Server 的 WriteTimeout 是 30 秒，
	// 长连接会在 30 秒后被静默切断，表现为"面板每隔半分钟重连一次"。
	// ResponseController 只影响这一条连接，不改全局配置。
	// 但完全取消截止时间也不行：匿名客户端只建连不读取时，写会永久阻塞，
	// 所以每次写之前再刷新一个较短的截止时间（见 send）。
	rc := http.NewResponseController(w)
	canDeadline := rc != nil && rc.SetWriteDeadline(time.Time{}) == nil
	refreshDeadline := func() {
		if canDeadline {
			_ = rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
		}
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	// 关掉 nginx 的响应缓冲，否则事件会被攒着一起发。
	h.Set("X-Accel-Buffering", "no")

	send := func(event string, rev uint64) bool {
		refreshDeadline()
		if _, err := fmt.Fprintf(w, "event: %s\ndata: {\"rev\":%d}\n\n", event, rev); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// retry 让浏览器在断线后 3 秒重连（默认可能更久）。
	refreshDeadline()
	if _, err := fmt.Fprint(w, "retry: 3000\n\n"); err != nil {
		return
	}
	if !send("hello", s.events.Revision()) {
		return
	}

	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			if !send("change", s.events.Revision()) {
				return
			}
		case <-ping.C:
			if !send("ping", s.events.Revision()) {
				return
			}
		}
	}
}

// liveNode 是「当前态」的轻量表示。
//
// 与 /api/nodes 的区别：后者带 22 个资产管理字段 + 令牌 + sparkline，
// 一次几 MB；这里只给渲染卡片需要的数值，一次几 KB。
// 对应哪吒那种"state 覆盖式当前态"的用法（见 docs/comparison-2026-10-20.md）。
type liveNode struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Group    string  `json:"group"`
	State    string  `json:"state"`
	CPU      float64 `json:"cpu"`
	MemPct   float64 `json:"memPct"`
	DiskPct  float64 `json:"diskPct"`
	Load1    float64 `json:"load1"`
	NetRx    uint64  `json:"netRx"` // B/s
	NetTx    uint64  `json:"netTx"` // B/s
	Uptime   uint64  `json:"uptime"`
	LastSeen int64   `json:"lastSeen"`
	Alerts   int     `json:"alerts"`
}

// handleLive 返回全部节点的当前态（轻量）。
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	views := s.buildNodeViewsLite()
	out := make([]liveNode, 0, len(views))
	alerts := s.st.ActiveAlertsByNode()
	for _, v := range views {
		out = append(out, liveNode{
			ID: v.ID, Name: v.Name, Group: v.Group, State: v.State,
			CPU:      round1(v.Latest.CPUPct),
			MemPct:   round1(pctF(v.Latest.MemUsed, v.Latest.MemTotal)),
			DiskPct:  round1(pctF(v.Latest.DiskUsed, v.Latest.DiskTotal)),
			Load1:    round2(v.Latest.Load1),
			NetRx:    v.Latest.NetRx,
			NetTx:    v.Latest.NetTx,
			Uptime:   v.Latest.Uptime,
			LastSeen: v.LastSeen,
			Alerts:   alerts[v.ID],
		})
	}
	// 版本号让客户端可以"只在变化时重绘"，不用自己算签名。
	writeJSON(w, http.StatusOK, map[string]any{
		"rev":   s.events.Revision(),
		"nodes": out,
	})
}

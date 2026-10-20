package web

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/store"
)

// TestEventHubNotifiesSubscribers 覆盖广播器本身：
// 订阅后一次 Publish 必须唤醒；无订阅者时不得阻塞；
// 已有未消费通知时再次 Publish 不能卡住（上报路径不能被慢客户端拖住）。
func TestEventHubNotifiesSubscribers(t *testing.T) {
	h := newEventHub()
	ch, cancel := h.subscribe()

	h.Publish()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("Publish 未唤醒订阅者")
	}

	// 不读通道，连续 Publish 必须立即返回（容量 1 + 非阻塞发送）
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			h.Publish()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("慢订阅者阻塞了 Publish —— 上报路径会被客户端拖死")
	}

	cancel()
	if h.subscriberCount() != 0 {
		t.Fatalf("取消订阅后仍有 %d 个订阅者", h.subscriberCount())
	}
	// 取消后 Publish 仍要正常
	h.Publish()
}

// TestStreamRequiresAuth 回归：事件流能给客户端"什么时候有数据"的信号，
// 未鉴权时必须拒绝，否则等于把机群活跃度暴露给任何人。
func TestStreamRequiresAuth(t *testing.T) {
	srv := New(store.New(t.TempDir()), nil, nil, Config{AdminToken: "stream-token-123456"})
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/stream", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("未鉴权访问事件流应 401，实际 %d", w.Code)
	}
}

// TestStreamSendsHelloAndChange 端到端验证 SSE：
// 首帧 hello 带版本号；数据变化后收到 change；写超时被清掉（长连接不会 30 秒被切断）。
func TestStreamSendsHelloAndChange(t *testing.T) {
	st := store.New(t.TempDir())
	const tok = "stream-token-123456"
	srv := New(st, nil, nil, Config{AdminToken: tok})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/stream", nil)
	req.Header.Set("X-Admin-Token", tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("建立事件流失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		t.Fatalf("事件流应 200，实际 %d: %s", resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type 应为 text/event-stream，实际 %q", ct)
	}

	br := bufio.NewReader(resp.Body)
	readEvent := func(want string) map[string]int64 {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			line, err := br.ReadString('\n')
			if err != nil {
				t.Fatalf("读取事件失败: %v", err)
			}
			if !strings.HasPrefix(line, "event: ") {
				continue
			}
			name := strings.TrimSpace(strings.TrimPrefix(line, "event: "))
			data, err := br.ReadString('\n')
			if err != nil {
				t.Fatalf("读取事件数据失败: %v", err)
			}
			if name != want {
				continue
			}
			var payload map[string]int64
			if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(data, "data: "))), &payload); err != nil {
				t.Fatalf("解析事件数据失败: %v (%q)", err, data)
			}
			return payload
		}
		t.Fatalf("未在超时内收到 %q 事件", want)
		return nil
	}

	if ev := readEvent("hello"); ev == nil {
		t.Fatal("首帧缺少版本号")
	}

	// 触发一次变化
	srv.events.Publish()
	if ev := readEvent("change"); ev == nil {
		t.Fatal("change 事件缺少版本号")
	}
}

// TestLiveReturnsCompactState 回归：
// /api/live 必须只给渲染需要的数值（不带令牌/价格/sparkline），
// 并且每个节点都能算出内存/磁盘百分比。
func TestLiveReturnsCompactState(t *testing.T) {
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "节点一", Group: "HK"})
	st.AddMetrics("n1", model.HostInfo{Hostname: "n1"}, model.Metrics{
		TS: time.Now().Unix(), CPUPct: 12.34,
		MemUsed: 512, MemTotal: 1024, DiskUsed: 25, DiskTotal: 100, NetRx: 100, NetTx: 50,
	})
	const tok = "live-token-123456"
	srv := New(st, nil, nil, Config{AdminToken: tok})
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/live", nil)
	req.Header.Set("X-Admin-Token", tok)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/api/live 应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		Rev   uint64 `json:"rev"`
		Nodes []struct {
			ID      string  `json:"id"`
			MemPct  float64 `json:"memPct"`
			DiskPct float64 `json:"diskPct"`
			CPU     float64 `json:"cpu"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(out.Nodes) != 1 {
		t.Fatalf("应有 1 个节点，实际 %d", len(out.Nodes))
	}
	n := out.Nodes[0]
	if n.MemPct < 49.9 || n.MemPct > 50.1 {
		t.Errorf("内存百分比错误: %v", n.MemPct)
	}
	if n.DiskPct < 24.9 || n.DiskPct > 25.1 {
		t.Errorf("磁盘百分比错误: %v", n.DiskPct)
	}
	// 不能泄露敏感/冗余字段
	body := w.Body.String()
	for _, bad := range []string{"token", "price", "spark", "note"} {
		if strings.Contains(body, `"`+bad+`"`) {
			t.Errorf("轻量端点不应包含 %q 字段: %s", bad, body)
		}
	}
}

// TestReportPublishesChange 回归：上报落地后必须广播，
// 否则面板只能等 30 秒兜底轮询，"实时"名不副实。
func TestReportPublishesChange(t *testing.T) {
	st := store.New(t.TempDir())
	n := &model.Node{ID: "n1", Name: "n1"}
	n.Token = "node-token-abcdef"
	st.UpsertNode(n)
	srv := New(st, nil, nil, Config{AdminToken: "report-token-123456"})
	h := srv.Handler()

	ch, cancel := srv.events.subscribe()
	defer cancel()

	body := `{"nodeId":"n1","host":{"hostname":"n1"},"metrics":{"cpuPct":10,"memUsed":1,"memTotal":2,"diskUsed":1,"diskTotal":2,"uptime":10}}`
	req := httptest.NewRequest(http.MethodPost, "/api/report", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Node-Token", n.Token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("上报失败: %d %s", w.Code, w.Body.String())
	}
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("上报后未广播变更 —— 面板无法实时刷新")
	}
}

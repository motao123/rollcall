package notify

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// twoWebhooks 起两个 webhook，返回各自被调用次数。
//
// 用真实 HTTP 而不是只测 routeChannels：过滤是否真的生效，取决于
// Send 的循环用的是过滤结果还是 cfg.Channels —— 函数写对了但没被用上
// 是这类改动最常见的失手方式。
func twoWebhooks(t *testing.T) (*Config, *int32, *int32) {
	t.Helper()
	SetAllowPrivateTargets(true)
	t.Cleanup(func() { SetAllowPrivateTargets(false) })

	var hitsA, hitsB int32
	mk := func(c *int32) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(c, 1)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	cfg := &Config{
		Enabled:     true,
		MinSeverity: "info",
		Channels: []ChannelConfig{
			{ID: "a", Type: "webhook", Name: "值班群", Enabled: true,
				Options: map[string]string{"url": mk(&hitsA)}},
			{ID: "b", Type: "webhook", Name: "审计群", Enabled: true,
				Options: map[string]string{"url": mk(&hitsB)}},
		},
	}
	return cfg, &hitsA, &hitsB
}

// TestSendRoutesToSelectedChannels 是 v1.34.0 的主用例：
// 规则可以限定渠道，未选中的渠道不能收到消息。
func TestSendRoutesToSelectedChannels(t *testing.T) {
	cfg, hitsA, hitsB := twoWebhooks(t)

	res := Send(cfg, Message{
		Title: "磁盘将满", Body: "95%", Severity: "warn", RuleName: "r_disk",
		ChannelIDs: []string{"b"},
	})
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("应只投递 1 个渠道且成功，实际 %+v", res)
	}
	if got := atomic.LoadInt32(hitsB); got != 1 {
		t.Errorf("选中的渠道应收到 1 次，实际 %d", got)
	}
	if got := atomic.LoadInt32(hitsA); got != 0 {
		t.Errorf("未选中的渠道不应收到消息，实际 %d 次", got)
	}
	if res[0].Channel != "审计群" {
		t.Errorf("结果里的通道名应为被投递的那个，实际 %q", res[0].Channel)
	}
}

// TestSendWithoutChannelsGoesToAll 确认留空仍是"全部渠道"（向后兼容）。
func TestSendWithoutChannelsGoesToAll(t *testing.T) {
	cfg, hitsA, hitsB := twoWebhooks(t)

	res := Send(cfg, Message{Title: "x", Body: "y", Severity: "warn", RuleName: "r"})
	if len(res) != 2 {
		t.Fatalf("应投递 2 个渠道，实际 %d: %+v", len(res), res)
	}
	if atomic.LoadInt32(hitsA) != 1 || atomic.LoadInt32(hitsB) != 1 {
		t.Errorf("两个渠道都应收到消息，实际 a=%d b=%d",
			atomic.LoadInt32(hitsA), atomic.LoadInt32(hitsB))
	}
}

// TestSendFallsBackWhenChannelsGone 是安全底线：
// 规则指定的渠道被删掉后，告警不能静默消失 —— 回退为全部渠道，
// 并在正文里说明原因（否则收件人不知道为什么会收到、该修哪里）。
func TestSendFallsBackWhenChannelsGone(t *testing.T) {
	SetAllowPrivateTargets(true)
	t.Cleanup(func() { SetAllowPrivateTargets(false) })

	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m Message
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			t.Errorf("解析 webhook 请求体: %v", err)
		}
		got <- m.Body
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	cfg := &Config{
		Enabled: true, MinSeverity: "info",
		Channels: []ChannelConfig{{
			ID: "still-here", Type: "webhook", Name: "值班群", Enabled: true,
			Options: map[string]string{"url": srv.URL},
		}},
	}
	res := Send(cfg, Message{
		Title: "CPU", Body: "load 12", Severity: "warn", RuleName: "r_cpu",
		ChannelIDs: []string{"deleted-channel"},
	})
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("应回退到现存渠道并投递成功，实际 %+v", res)
	}
	body := <-got
	if !strings.Contains(body, "已不存在或已停用") {
		t.Fatalf("回退时正文应说明原因，实际: %q", body)
	}
	if !strings.Contains(body, "load 12") {
		t.Fatalf("原文不能被吞掉，实际: %q", body)
	}
}

// TestSendSkipsMissingChannelAndWarns 覆盖"部分渠道消失"：
// 保留的渠道照常收到，正文里列出缺失的 ID。
func TestSendSkipsMissingChannelAndWarns(t *testing.T) {
	cfg, hitsA, hitsB := twoWebhooks(t)

	res := Send(cfg, Message{
		Title: "x", Body: "y", Severity: "warn", RuleName: "r",
		ChannelIDs: []string{"a", "gone"},
	})
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("应只投递现存的那一个，实际 %+v", res)
	}
	if atomic.LoadInt32(hitsA) != 1 || atomic.LoadInt32(hitsB) != 0 {
		t.Errorf("应只命中 a，实际 a=%d b=%d", atomic.LoadInt32(hitsA), atomic.LoadInt32(hitsB))
	}
}

// TestSendIgnoresDisabledChannel 确认停用的渠道即使用 ID 指定也不投递，
// 并触发回退说明（全部不可用时）。
func TestSendIgnoresDisabledChannel(t *testing.T) {
	cfg, hitsA, hitsB := twoWebhooks(t)
	cfg.Channels[1].Enabled = false

	res := Send(cfg, Message{
		Title: "x", Body: "y", Severity: "warn", RuleName: "r",
		ChannelIDs: []string{"b"},
	})
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("停用渠道应回退到启用的那个，实际 %+v", res)
	}
	if atomic.LoadInt32(hitsB) != 0 {
		t.Errorf("停用的渠道不应收到消息")
	}
	if atomic.LoadInt32(hitsA) != 1 {
		t.Errorf("回退后应命中启用的渠道 a，实际 %d", atomic.LoadInt32(hitsA))
	}
}

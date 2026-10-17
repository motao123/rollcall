package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/motao/rollcall/internal/model"
)

// TestSaveRacesWithWrites 是回归测试：Save() 必须在持锁期间完成深拷贝。
//
// 曾经的缺陷：快照里直接引用 s.nodes / s.hosts / s.settings 等 map，
// 解锁后才 json.Marshal，marshal 迭代 map 时与写入 goroutine 撞车，
// 触发不可 recover 的 "fatal error: concurrent map iteration and map write"，
// 整个进程死亡。StartAutoSave 每 60 秒触发一次，因此这是必然事件。
//
// 注意：Go runtime 的 map 并发检测**不依赖 -race**，
// 因此在没有 gcc / CGO 的机器上同样能捕获此缺陷。
func TestSaveRacesWithWrites(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)

	for i := 0; i < 40; i++ {
		s.UpsertNode(&model.Node{ID: NewID("n"), Name: "node", Token: NewToken()})
	}
	for _, sv := range []*model.Service{{ID: NewID("s"), Name: "svc", Type: "tcp", Target: "1.2.3.4:80"}} {
		s.UpsertService(sv)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ruleIDs := []string{"r_a", "r_b", "r_c"}
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			nodes := s.Nodes()
			if len(nodes) > 0 {
				n := nodes[i%len(nodes)]
				// 写 hosts map —— 与 marshal 迭代直接冲突
				s.AddMetrics(n.ID, model.HostInfo{Hostname: "h"}, model.Metrics{TS: time.Now().UnixNano()})
			}
			s.SetSetting("notify", `{"enabled":true}`)
			s.UpsertRule(&model.AlertRule{ID: ruleIDs[i%len(ruleIDs)], Name: "r", Enabled: true})
			// 让出调度：真实场景是 30s 一次上报，紧循环会饿死 Save 的 RLock
			if i%64 == 63 {
				time.Sleep(100 * time.Microsecond)
			}
		}
	}()

	for i := 0; i < 300; i++ {
		if err := s.Save(); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("Save 返回错误: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}

// TestSavePersistsAllSections 确保深拷贝重构没有丢字段。
func TestSavePersistsAllSections(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)

	n := &model.Node{ID: "n1", Name: "节点一", Provider: "test", Token: "tok-secret"}
	s.UpsertNode(n)
	s.AddMetrics("n1", model.HostInfo{Hostname: "h1", OS: "Debian 12"}, model.Metrics{TS: 1000, CPUPct: 42.5})
	s.UpsertService(&model.Service{ID: "s1", Name: "svc", Type: "tcp", Target: "1.2.3.4:80", Interval: 60, Enabled: true})
	s.AddServiceResult(model.ServiceResult{ServiceID: "s1", TS: 1000, OK: true, LatencyMs: 12})
	s.UpsertRule(&model.AlertRule{ID: "r1", Name: "规则一", Metric: "cpu", Op: "gt", Threshold: 90, Enabled: true})
	s.OpenAlert(&model.Alert{ID: "a1", RuleID: "r1", NodeID: "n1", Severity: "warn", StartedAt: 1000})
	s.SetSetting("notify", `{"enabled":true}`)

	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("读快照: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("解析快照: %v", err)
	}

	for _, key := range []string{"nodes", "hosts", "series", "services", "svcRes", "rules", "alerts", "tokens", "settings"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("快照缺少字段 %q", key)
		}
	}

	// 重新加载，验证内容完整（尤其 tokens 回填）
	s2 := New(dir)
	got, ok := s2.Node("n1")
	if !ok {
		t.Fatal("重载后节点丢失")
	}
	if got.Token != "tok-secret" {
		t.Errorf("令牌未持久化: got %q, want %q", got.Token, "tok-secret")
	}
	if got.Name != "节点一" || got.Provider != "test" {
		t.Errorf("节点字段丢失: %+v", got)
	}
	if h := s2.Host("n1"); h.Hostname != "h1" || h.OS != "Debian 12" {
		t.Errorf("host 丢失: %+v", h)
	}
	if m, ts, ok := s2.Latest("n1"); !ok || m.CPUPct != 42.5 || ts != 1000 {
		t.Errorf("指标丢失: %+v ts=%d ok=%v", m, ts, ok)
	}
	if len(s2.Services()) != 1 {
		t.Errorf("服务丢失: %d", len(s2.Services()))
	}
	if len(s2.Rules()) != 1 {
		t.Errorf("规则丢失: %d", len(s2.Rules()))
	}
	if len(s2.Alerts(0)) != 1 {
		t.Errorf("告警丢失: %d", len(s2.Alerts(0)))
	}
	if s2.GetSetting("notify") != `{"enabled":true}` {
		t.Errorf("settings 丢失: %q", s2.GetSetting("notify"))
	}

	// 令牌不得出现在公开视图（Nodes() 走 json:"-"）
	pub, _ := json.Marshal(s2.Nodes())
	if strings.Contains(string(pub), "tok-secret") {
		t.Error("令牌泄露到公开节点视图")
	}
}

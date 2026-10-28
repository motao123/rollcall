package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/motao123/rollcall/internal/agentwire"
	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/store"
)

// TestAgentCapsWire 覆盖能力上报的三条路径：
//
//	发 "none" → 记 "none"（已确认无能力）
//	发 "temp" → 记 "temp"
//	不发头    → 保持空（能力未知，不能被"确认无能力"覆盖）
//
// 第三条是关键：旧 Agent 升级前后会混跑，若把"没发头"写成 none，
// 面板会对这些机器断言"没有传感器"——那是没有依据的结论。
func TestAgentCapsWire(t *testing.T) {
	const adminTok = "caps-test-token-1234567890"
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "无传感器机", Token: "tok-n1"})
	st.UpsertNode(&model.Node{ID: "n2", Name: "有传感器机", Token: "tok-n2"})
	st.UpsertNode(&model.Node{ID: "n3", Name: "旧 Agent", Token: "tok-n3"})
	srv := New(st, nil, nil, Config{AdminToken: adminTok})
	h := srv.Handler()

	post := func(nodeID, token, caps string) {
		t.Helper()
		body := `{"nodeId":"` + nodeID + `","host":{"hostname":"h"},"metrics":{}}`
		req := httptest.NewRequest(http.MethodPost, "/api/report", strings.NewReader(body))
		req.Header.Set("X-Node-Token", token)
		if caps != "" {
			req.Header.Set(agentwire.HeaderAgentCaps, caps)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("上报 %s 失败: %d %s", nodeID, w.Code, w.Body.String())
		}
	}

	post("n1", "tok-n1", agentwire.CapsNone)
	post("n2", "tok-n2", "Temp")
	post("n3", "tok-n3", "") // 旧 Agent：不带这个头

	want := map[string]string{"n1": "none", "n2": "temp", "n3": ""}
	for id, exp := range want {
		n, ok := st.Node(id)
		if !ok {
			t.Fatalf("节点 %s 不存在", id)
		}
		if n.Caps != exp {
			t.Errorf("节点 %s 的 caps = %q，期望 %q", id, n.Caps, exp)
		}
	}

	// 详情接口要把它带给前端（面板靠它决定怎么陈述"没有温度"）
	req := httptest.NewRequest(http.MethodGet, "/api/nodes/n2", nil)
	req.Header.Set("X-Admin-Token", adminTok)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var detail struct {
		Node struct {
			Caps string `json:"caps"`
		} `json:"node"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatalf("详情响应不是 JSON: %v", err)
	}
	if detail.Node.Caps != "temp" {
		t.Fatalf("详情未带出 caps: %q", detail.Node.Caps)
	}
}

// TestAgentCapsPersistAndNotDowngraded 覆盖两点：
//
//  1. 能力落盘后重启仍在（json 标签正常序列化，不是 json:"-" 那类坑）；
//  2. 旧 Agent 的"无头"上报**不会**把已记录的能力抹成空。
func TestAgentCapsPersistAndNotDowngraded(t *testing.T) {
	dir := t.TempDir()
	st := store.New(dir)
	st.UpsertNode(&model.Node{ID: "n1", Name: "n1", Token: "tok-n1"})
	// 直接走 store 写入（等价于服务端解析完头之后的一步）
	st.SetNodeCaps("n1", agentwire.NormalizeCaps([]string{"temp"}))
	// 模拟"旧 Agent 上报"：调用方在头缺席时根本不会调用它；
	// 这里显式传空，验证 store 不会把它当成"清空能力"。
	st.SetNodeCaps("n1", "")

	n, _ := st.Node("n1")
	if n.Caps != "temp" {
		t.Fatalf("空值把已知能力降级成了未知: %q", n.Caps)
	}

	// 落盘 + 重新加载
	if err := st.Save(); err != nil {
		t.Fatalf("保存快照失败: %v", err)
	}
	st2 := store.New(dir)
	if err := st2.LoadErr(); err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	n2, ok := st2.Node("n1")
	if !ok {
		t.Fatal("重启后节点丢失")
	}
	if n2.Caps != "temp" {
		t.Fatalf("重启后能力丢失: %q（可能是字段没进快照）", n2.Caps)
	}
}

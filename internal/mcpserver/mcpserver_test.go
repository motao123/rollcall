package mcpserver

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestBackend 起一个记录鉴权头的假面板，按路径返回固定 JSON。
func newTestBackend(t *testing.T, gotAuth *string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/overview", func(w http.ResponseWriter, r *http.Request) {
		*gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"nodes":13,"online":13,"remainingValue":78.27}`)
	})
	mux.HandleFunc("/api/nodes/n1", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(401)
			return
		}
		io.WriteString(w, `{"id":"n1","name":"racknerd-dc02"}`)
	})
	mux.HandleFunc("/api/nodes/n1/series", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"range":"`+r.URL.Query().Get("range")+`","points":2}`)
	})
	mux.HandleFunc("/api/assets", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"monthlyTotal":13.65,"remainingTotal":78.27}`)
	})
	mux.HandleFunc("/api/bad", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		io.WriteString(w, `boom`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func callLine(s *Server, line string) map[string]any {
	t := s.handleLine([]byte(line))
	if t == nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(t, &m); err != nil {
		return map[string]any{"__unmarshal_error": err.Error()}
	}
	return m
}

func TestInitializeEchoesProtocolVersion(t *testing.T) {
	var auth string
	srv := newTestBackend(t, &auth)
	s := New(Options{BaseURL: srv.URL, Token: "rct_test"})
	resp := callLine(s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`)
	if e, ok := resp["error"]; ok {
		t.Fatalf("initialize 不应报错: %v", e)
	}
	res := resp["result"].(map[string]any)
	if res["protocolVersion"] != "2024-11-05" {
		t.Fatalf("应回显客户端协议版本，实际 %v", res["protocolVersion"])
	}
	if res["serverInfo"].(map[string]any)["name"] != "rollcall-mcp" {
		t.Fatalf("serverInfo.name 错误: %v", res["serverInfo"])
	}
	if _, ok := res["capabilities"].(map[string]any)["tools"]; !ok {
		t.Fatalf("capabilities 应声明 tools: %v", res["capabilities"])
	}
	// 没带版本时用默认
	resp2 := callLine(s, `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{}}`)
	if resp2["result"].(map[string]any)["protocolVersion"] != protoDefault {
		t.Fatalf("缺省协议版本应为 %s", protoDefault)
	}
}

func TestToolsListAndCall(t *testing.T) {
	var auth string
	srv := newTestBackend(t, &auth)
	s := New(Options{BaseURL: srv.URL, Token: "rct_test"})

	list := callLine(s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	tools := list["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 7 {
		t.Fatalf("应有 7 个工具，实际 %d", len(tools))
	}
	names := map[string]bool{}
	for _, tl := range tools {
		tm := tl.(map[string]any)
		names[tm["name"].(string)] = true
		if _, ok := tm["inputSchema"]; !ok {
			t.Fatalf("工具 %v 缺 inputSchema", tm["name"])
		}
	}
	for _, want := range []string{"get_overview", "list_nodes", "get_node", "get_node_series", "list_services", "list_alerts", "list_assets"} {
		if !names[want] {
			t.Fatalf("缺少工具 %s", want)
		}
	}

	// 正常调用：透传 Bearer、返回美化 JSON
	call := callLine(s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_overview"}}`)
	res := call["result"].(map[string]any)
	if res["isError"] == true {
		t.Fatalf("get_overview 不应失败: %v", res)
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, `"online": 13`) {
		t.Fatalf("应返回美化 JSON，实际 %q", text)
	}
	if auth != "Bearer rct_test" {
		t.Fatalf("应透传 Bearer 令牌，实际 %q", auth)
	}
	// id 应原样回显（字符串 id）
	if call["id"] != "2" && call["id"].(float64) != 2 {
		t.Fatalf("id 应回显，实际 %v", call["id"])
	}
}

func TestToolsCallValidation(t *testing.T) {
	var auth string
	srv := newTestBackend(t, &auth)
	s := New(Options{BaseURL: srv.URL, Token: "rct_test"})

	// 未知工具 → JSON-RPC 错误
	r := callLine(s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"rm_rf"}}`)
	if r["error"] == nil {
		t.Fatalf("未知工具应报错: %v", r)
	}
	// 节点 id 越权（路径穿越）→ isError 结果
	r2 := callLine(s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_node","arguments":{"id":"../admin"}}}`)
	res := r2["result"].(map[string]any)
	if res["isError"] != true {
		t.Fatalf("非法 id 应返回 isError: %v", res)
	}
	// 非法 range → isError
	r3 := callLine(s, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_node_series","arguments":{"id":"n1","range":"1s"}}}`)
	if r3["result"].(map[string]any)["isError"] != true {
		t.Fatalf("非法 range 应返回 isError: %v", r3)
	}
	// 合法 range 透传查询串
	r4 := callLine(s, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"get_node_series","arguments":{"id":"n1","range":"7d"}}}`)
	text := r4["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, `"range": "7d"`) {
		t.Fatalf("range 应透传到查询串，实际 %q", text)
	}
}

func TestProtocolEdges(t *testing.T) {
	var auth string
	srv := newTestBackend(t, &auth)
	s := New(Options{BaseURL: srv.URL, Token: "rct_test"})

	// 通知（无 id）不应答
	if resp := s.handleLine([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); resp != nil {
		t.Fatalf("通知不应答，实际 %s", resp)
	}
	// ping → {}
	p := callLine(s, `{"jsonrpc":"2.0","id":9,"method":"ping"}`)
	if _, ok := p["result"].(map[string]any); !ok || len(p["result"].(map[string]any)) != 0 {
		t.Fatalf("ping 应返回空对象: %v", p)
	}
	// 未知方法
	u := callLine(s, `{"jsonrpc":"2.0","id":10,"method":"resources/list"}`)
	if u["error"] == nil {
		t.Fatalf("未知方法应报 -32601: %v", u)
	}
	// 非法 JSON
	bad := callLine(s, `{not json`)
	if bad["error"] == nil {
		t.Fatalf("非法 JSON 应报 -32700: %v", bad)
	}
	// 面板 5xx → isError 而非协议错误
	s5 := New(Options{BaseURL: srv.URL, Token: "rct_test"})
	// 借用 list_assets 工具打 /api/assets（正常），再直接构造打 5xx 的调用：
	r := callLine(s5, `{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"get_overview"}}`)
	_ = r
	// 面板错误路径：把 BaseURL 指向 /api/bad 所在服务的根 —— 直接用 Client 验证更聚焦
	c := &Client{BaseURL: srv.URL, Token: "rct_test", HC: srv.Client()}
	if _, err := c.Get("/api/bad"); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("5xx 应报错，实际 %v", err)
	}
}

func TestRunStdioLoop(t *testing.T) {
	var auth string
	srv := newTestBackend(t, &auth)
	s := New(Options{BaseURL: srv.URL, Token: "rct_test"})
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_assets"}}`,
		``,
	}, "\n")
	var out bytes.Buffer
	s.Run(strings.NewReader(in), &out)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("应输出 2 行应答（通知不回），实际 %d 行: %q", len(lines), out.String())
	}
	if !strings.Contains(lines[1], `remainingTotal\": 78.27`) {
		t.Fatalf("list_assets 结果不符: %q", lines[1])
	}
}

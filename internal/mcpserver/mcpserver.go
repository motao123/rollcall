// Package mcpserver 实现最小只读 MCP 服务器（stdio 传输）。
//
// MCP（Model Context Protocol）在 stdio 传输上是「换行分帧的 JSON-RPC 2.0」：
// 每条消息一行紧凑 JSON。本包只依赖标准库，与项目零第三方依赖的约束一致。
//
// 设计原则：只读。所有工具都映射到面板的 authReadonly GET 端点，
// 鉴权用作用域令牌（Authorization: Bearer rct_...），与 Agent 单向边界
// 无关——MCP 服务器是面板的另一个 HTTP 客户端，不是节点的控制通道。
package mcpserver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/motao123/rollcall/internal/version"
)

// Options 配置 MCP 服务器。BaseURL 与 Token 二选一不可省略。
type Options struct {
	BaseURL string // 面板地址，如 http://127.0.0.1:19080
	Token   string // rct_ 开头的作用域令牌；只读用途建议 scopes=["read"]
	Client  *http.Client
}

// tool 是一个 MCP 工具：名称、描述、JSON Schema 与执行函数。
type tool struct {
	Name        string
	Description string
	Schema      string // inputSchema 的 JSON 文本
	Run         func(c *Client, args map[string]any) (string, error)
}

// content 是 MCP 工具结果里的内容块。
type content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Server 持有工具注册表与面板客户端。
type Server struct {
	client *Client
	tools  []tool
}

// New 构造服务器。opts.Client 为 nil 时使用 10 秒超时的默认客户端。
func New(opts Options) *Server {
	hc := opts.Client
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	s := &Server{client: &Client{BaseURL: strings.TrimRight(opts.BaseURL, "/"), Token: opts.Token, HC: hc}}
	s.tools = []tool{
		{
			Name:        "get_overview",
			Description: "机群总览：节点/在线/离线/活跃告警、平均 CPU 与内存、月度成本与剩余价值（USD）、累计流量",
			Schema:      `{"type":"object","properties":{},"additionalProperties":false}`,
			Run:         func(c *Client, _ map[string]any) (string, error) { return c.Get("/api/overview") },
		},
		{
			Name:        "list_nodes",
			Description: "全部节点列表：状态、CPU/内存/磁盘使用率、运行时长、Agent 版本、分组/服务商/到期日",
			Schema:      `{"type":"object","properties":{},"additionalProperties":false}`,
			Run:         func(c *Client, _ map[string]any) (string, error) { return c.Get("/api/nodes") },
		},
		{
			Name:        "get_node",
			Description: "单台节点详情：系统信息、最新指标、资产与流量配额。参数 id 取自 list_nodes",
			Schema:      `{"type":"object","properties":{"id":{"type":"string","description":"节点 ID"}},"required":["id"],"additionalProperties":false}`,
			Run: func(c *Client, args map[string]any) (string, error) {
				id, err := nodeID(args)
				if err != nil {
					return "", err
				}
				return c.Get("/api/nodes/" + id)
			},
		},
		{
			Name:        "get_node_series",
			Description: "单台节点的历史时序（CPU/内存/磁盘/网络/负载）。range 为 1h|6h|24h|7d|30d|365d，长范围自动按 1m/5m/1h 聚合",
			Schema:      `{"type":"object","properties":{"id":{"type":"string"},"range":{"type":"string","enum":["1h","6h","24h","7d","30d","365d"],"default":"24h"}},"required":["id"],"additionalProperties":false}`,
			Run: func(c *Client, args map[string]any) (string, error) {
				id, err := nodeID(args)
				if err != nil {
					return "", err
				}
				rng := "24h"
				if v, ok := args["range"].(string); ok && v != "" {
					if !validRange(v) {
						return "", fmt.Errorf("range 仅支持 1h/6h/24h/7d/30d/365d，收到 %q", v)
					}
					rng = v
				}
				return c.Get("/api/nodes/" + id + "/series?range=" + rng)
			},
		},
		{
			Name:        "list_services",
			Description: "服务探测列表：类型（HTTP/TCP/DNS/ICMP）、最近延迟、丢包/抖动、证书剩余天数、24h 可用率",
			Schema:      `{"type":"object","properties":{},"additionalProperties":false}`,
			Run:         func(c *Client, _ map[string]any) (string, error) { return c.Get("/api/services") },
		},
		{
			Name:        "list_alerts",
			Description: "告警近况：活跃数、最近记录（最多 200 条）、静默与通知队列状态",
			Schema:      `{"type":"object","properties":{},"additionalProperties":false}`,
			Run:         func(c *Client, _ map[string]any) (string, error) { return c.Get("/api/alerts") },
		},
		{
			Name:        "list_assets",
			Description: "资产台账：到期日与剩余天数、年付价与币种、月摊与剩余价值（USD），按到期时间排序",
			Schema:      `{"type":"object","properties":{},"additionalProperties":false}`,
			Run:         func(c *Client, _ map[string]any) (string, error) { return c.Get("/api/assets") },
		},
	}
	return s
}

// nodeID 校验并返回可安全拼进 URL 路径的节点 ID。
// 面板侧也有路径清洗，但客户端先把关，避免构造出歧义请求。
func nodeID(args map[string]any) (string, error) {
	id, _ := args["id"].(string)
	id = strings.TrimSpace(id)
	if id == "" || strings.ContainsAny(id, "/?#") || strings.Contains(id, "..") {
		return "", fmt.Errorf("id 无效：需要 list_nodes 返回的节点 ID")
	}
	return url.PathEscape(id), nil
}

func validRange(v string) bool {
	switch v {
	case "1h", "6h", "24h", "7d", "30d", "365d":
		return true
	}
	return false
}

// Client 是面板只读 API 的极薄封装。
type Client struct {
	BaseURL string
	Token   string
	HC      *http.Client
}

// Get 请求路径并返回美化后的 JSON 文本。非 2xx 一律报错。
func (c *Client) Get(path string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HC.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return "", fmt.Errorf("面板返回 %d: %s", resp.StatusCode, msg)
	}
	var buf bytes.Buffer
	if json.Indent(&buf, body, "", "  ") == nil {
		return buf.String(), nil
	}
	return string(body), nil
}

/* ---------------- JSON-RPC / MCP 协议层 ---------------- */

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

const (
	errParse     = -32700
	errMethod    = -32601
	errInvalid   = -32602
	errInternal  = -32603
	protoDefault = "2025-06-18"
)

// handleLine 处理一行输入。返回 nil 表示通知（无需应答）。
func (s *Server) handleLine(line []byte) []byte {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil || req.JSONRPC != "2.0" {
		return mustJSON(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &rpcError{Code: errParse, Message: "parse error: 不是合法的 JSON-RPC 2.0 消息"}})
	}
	// 通知（无 id）只可能来自客户端的 initialized/cancelled，忽略即可。
	if len(req.ID) == 0 {
		return nil
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		proto := protoDefault
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(req.Params, &p) == nil && p.ProtocolVersion != "" {
			// 回显客户端请求的版本：客户端不支持时由它决定断开，
			// 服务器单方面报新版反而把老客户端挡在门外。
			proto = p.ProtocolVersion
		}
		resp.Result = map[string]any{
			"protocolVersion": proto,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "rollcall-mcp", "version": version.Version},
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		tools := make([]map[string]any, 0, len(s.tools))
		for _, t := range s.tools {
			var schema any
			if json.Unmarshal([]byte(t.Schema), &schema) != nil {
				schema = map[string]any{"type": "object"}
			}
			tools = append(tools, map[string]any{
				"name": t.Name, "description": t.Description, "inputSchema": schema,
			})
		}
		resp.Result = map[string]any{"tools": tools}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments,omitempty"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
			resp.Error = &rpcError{Code: errInvalid, Message: "params 无效：需要 name"}
			return mustJSON(resp)
		}
		var found *tool
		for i := range s.tools {
			if s.tools[i].Name == p.Name {
				found = &s.tools[i]
				break
			}
		}
		if found == nil {
			resp.Error = &rpcError{Code: errInvalid, Message: "unknown tool: " + p.Name}
			return mustJSON(resp)
		}
		args := map[string]any{}
		if len(p.Arguments) > 0 && string(p.Arguments) != "null" {
			if err := json.Unmarshal(p.Arguments, &args); err != nil {
				resp.Error = &rpcError{Code: errInvalid, Message: "arguments 不是合法 JSON 对象"}
				return mustJSON(resp)
			}
		}
		text, err := found.Run(s.client, args)
		// 工具执行失败按 MCP 语义返回 isError 结果（协议层没坏），
		// 让客户端模型能看到错误文本并自行决定下一步。
		if err != nil {
			resp.Result = map[string]any{
				"content": []content{{Type: "text", Text: "工具执行失败: " + err.Error()}},
				"isError": true,
			}
		} else {
			resp.Result = map[string]any{"content": []content{{Type: "text", Text: text}}}
		}
	default:
		resp.Error = &rpcError{Code: errMethod, Message: "method not found: " + req.Method}
	}
	return mustJSON(resp)
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// rpcResponse 的字段全部可序列化，到不了这里；防御性兜底。
		return []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"internal error"}}`)
	}
	return b
}

// Run 进入 stdio 循环：每行一条消息，应答单行 JSON。
// 日志一律走 stderr——stdout 是协议通道，混入即损坏帧。
func (s *Server) Run(in io.Reader, out io.Writer) {
	w := bufio.NewWriter(out)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if resp := s.handleLine(line); resp != nil {
			w.Write(resp)
			w.WriteByte('\n')
			w.Flush()
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "rollcall-mcp: 读取 stdin 结束: %v\n", err)
	}
}

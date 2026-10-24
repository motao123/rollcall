// rollcall-mcp 是 Rollcall 面板的只读 MCP 服务器（stdio 传输）。
//
// 它是面板的一个普通 HTTP 客户端：用作用域令牌调用只读 API，
// 不接触节点、不下发任何指令。配置示例（Claude Desktop 等）：
//
//	{
//	  "mcpServers": {
//	    "rollcall": {
//	      "command": "rollcall-mcp",
//	      "env": {
//	        "ROLLCALL_URL": "http://127.0.0.1:19080",
//	        "ROLLCALL_TOKEN": "rct_..."
//	      }
//	    }
//	  }
//	}
//
// 令牌建议在面板「设置 → API 令牌」创建：scopes=["read"]，
// paths 限定到 /api/overview、/api/nodes、/api/services、/api/alerts、/api/assets。
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/motao123/rollcall/internal/mcpserver"
)

func main() {
	baseURL := flag.String("url", os.Getenv("ROLLCALL_URL"), "面板地址（默认取环境变量 ROLLCALL_URL）")
	token := flag.String("token", os.Getenv("ROLLCALL_TOKEN"), "作用域令牌 rct_...（默认取环境变量 ROLLCALL_TOKEN）")
	flag.Parse()

	if *baseURL == "" || *token == "" {
		fmt.Fprintln(os.Stderr, "rollcall-mcp: 需要 -url/-token 或环境变量 ROLLCALL_URL/ROLLCALL_TOKEN")
		os.Exit(2)
	}
	// 只认 rct_ 令牌：Bearer 通道只接受作用域令牌，
	// 提前拒绝能避免用户误把管理令牌塞进 MCP 客户端配置里明文存放。
	if len(*token) < 8 || (*token)[:4] != "rct_" {
		fmt.Fprintln(os.Stderr, "rollcall-mcp: token 应为 rct_ 开头的作用域令牌（面板「设置 → API 令牌」创建，scopes 选 read）")
		os.Exit(2)
	}

	mcpserver.New(mcpserver.Options{BaseURL: *baseURL, Token: *token}).Run(os.Stdin, os.Stdout)
}

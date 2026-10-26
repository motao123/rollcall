package web

// 面板对外地址（域名反代场景）。
//
// 反代之后，浏览器看到的地址（https://monitor.example.com）与服务端看到的不一定相同：
// 服务端可能只听 127.0.0.1:19080，或者收到的是内网地址。这个值用于两处会「离开
// 当前浏览器」的场景：
//   - 告警/日报通知里的面板链接（收件人点开要能访问）
//   - Agent 接入命令里的 server 地址（在别的机器上执行）
//
// 它以前只能靠启动参数 -panel-link 设置，反代用户改一次地址就得改 systemd 单元；
// 现在面板里可改并落盘，启动参数退化为默认值。

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// PanelLink 返回当前的面板对外地址（可能为空 = 未设置）。
func (s *Server) PanelLink() string {
	s.panelLinkMu.RLock()
	defer s.panelLinkMu.RUnlock()
	return s.panelLink
}

// SetPanelLink 更新对外地址并落盘，同时同步给告警引擎（通知链接由它拼）。
func (s *Server) SetPanelLink(link string) {
	s.panelLinkMu.Lock()
	s.panelLink = link
	s.panelLinkMu.Unlock()
	if s.engine != nil {
		s.engine.SetLink(link)
	}
	s.st.SetSetting("panel_link", link)
}

// safePanelHost 判断主机名是否只含域名/IP/端口的合法字符。
//
// 为什么需要：url.Parse 会放行 Host 里的 `;`、`$`、`()` 等字符
// （实测 `https://a.com$(id)` 解析为 Host="a.com$(id)"），而这个地址会被
// agentInstallCommand 原样拼进「节点接入命令」，由管理员复制到目标服务器的
// shell 里执行。白名单只放行域名/IPv4/IPv6 括号/端口可能出现的字符，
// 从源头堵住命令替换，双保险的另一层在命令拼接处的单引号包裹。
func safePanelHost(host string) bool {
	if host == "" {
		return false
	}
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == ':' || r == '[' || r == ']':
		default:
			return false
		}
	}
	return true
}

// normalizePanelLink 校验并规范化对外地址。
//
// 只接受 http/https 的「站点根」：路径必须为空或 "/"。
// 子路径部署（https://example.com/rollcall/）需要服务端整体支持 basePath，
// 目前不支持 —— 明确拒绝比默默生成一堆打不开的链接好。
func normalizePanelLink(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "地址格式无效：" + err.Error()
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "地址必须以 http:// 或 https:// 开头"
	}
	if u.Host == "" {
		return "", "地址缺少主机名（例如 https://monitor.example.com）"
	}
	if !safePanelHost(u.Host) {
		return "", "主机名只能包含字母、数字、点、连字符、冒号与方括号（IPv6）"
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", "地址不能带查询参数或锚点"
	}
	if p := strings.Trim(u.Path, "/"); p != "" {
		return "", "暂不支持子路径部署（如 /rollcall/）：请用独立域名或子域名，根路径反代到本服务"
	}
	u.Path = ""
	return u.Scheme + "://" + u.Host, ""
}

// handlePanelLink 读写面板对外地址。
func (s *Server) handlePanelLink(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]string{"panelLink": s.PanelLink()})

	case http.MethodPost:
		body := http.MaxBytesReader(w, r.Body, 4<<10)
		var in struct {
			Link string `json:"link"`
		}
		dec := json.NewDecoder(body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			fail(w, http.StatusBadRequest, "bad json: "+err.Error())
			return
		}
		link, msg := normalizePanelLink(in.Link)
		if msg != "" {
			fail(w, http.StatusBadRequest, msg)
			return
		}
		s.SetPanelLink(link)
		writeJSON(w, http.StatusOK, map[string]string{"panelLink": link})

	default:
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

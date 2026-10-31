package web

// Nezha 一键迁移：拉取 Nezha 主控的服务器清单，映射成批量导入规格后
// 走与粘贴导入完全相同的落库路径（applyNodeImport）。
//
// 目的：把"替代 Nezha"的第一步——几十台节点的台账录入——压成一次粘贴
// 面板地址与管理令牌。Agent 的接入仍按节点逐台执行安装命令；
// Rollcall 不导入 Nezha 的任何监控数据，指标由自家 Agent 重新采集。
//
// 安全边界：
//   - 仅管理员可用（路由挂 authAdmin），审计自动记录 admin.create
//   - Nezha 令牌只在本次请求内存中使用：不落盘、不进审计、不回显到响应
//   - 目标 URL 仅接受 http(s) 且不含内嵌凭据；响应用 8MB 上限防被当代理拉大文件
//   - 与既有节点同名时跳过而不是覆盖——覆盖台账是恢复备份才该做的事

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/motao123/rollcall/internal/sanitize"
)

type nezhaImportRequest struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// nezhaServer 用宽松字段映射：Nezha 各版本的 host JSON 键大小写与下划线
// 不统一（CountryCode / country_code），按不区分大小写的键名查找。
type nezhaServer struct {
	Name string         `json:"name"`
	Note string         `json:"note"`
	Host map[string]any `json:"host"`
}

func nezhaCountry(host map[string]any) string {
	for k, v := range host {
		if strings.EqualFold(k, "countrycode") || strings.EqualFold(k, "country_code") {
			s, _ := v.(string)
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func (s *Server) handleNezhaImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	body := http.MaxBytesReader(w, r.Body, 16<<10)
	var in nezhaImportRequest
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	in.URL = strings.TrimSpace(in.URL)
	in.URL = strings.TrimRight(in.URL, "/")
	if in.URL == "" || in.Token == "" {
		fail(w, http.StatusBadRequest, "url 与 token 均为必填")
		return
	}
	u, err := url.Parse(in.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		fail(w, http.StatusBadRequest, "url 必须以 http:// 或 https:// 开头")
		return
	}
	if u.User != nil {
		fail(w, http.StatusBadRequest, "url 不应包含内嵌凭据")
		return
	}

	// 拉取 Nezha 服务器清单（v1 API：GET /api/v1/server，Bearer 管理令牌）
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, in.URL+"/api/v1/server", nil)
	if err != nil {
		fail(w, http.StatusBadRequest, "构造请求失败: "+err.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+in.Token)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		fail(w, http.StatusBadGateway, "无法访问 Nezha 主控: "+err.Error())
		return
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		fail(w, http.StatusBadGateway, "读取 Nezha 响应失败: "+err.Error())
		return
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		fail(w, http.StatusBadGateway,
			fmt.Sprintf("Nezha 返回 %d（令牌错误或权限不足）: %s", resp.StatusCode, msg))
		return
	}

	var payload struct {
		Code    int           `json:"code"`
		Message string        `json:"message"`
		Result  []nezhaServer `json:"result"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		fail(w, http.StatusBadGateway, "Nezha 响应不是预期的 JSON 结构: "+err.Error())
		return
	}
	// v1 API：code=0 为成功；非 0 时 message 带原因（令牌错误等）
	if payload.Result == nil || (payload.Code != 0 && payload.Code != 200 && payload.Code != http.StatusOK) {
		msg := payload.Message
		if msg == "" {
			msg = "result 为空"
		}
		fail(w, http.StatusBadGateway,
			fmt.Sprintf("Nezha 返回 code=%d: %s", payload.Code, msg))
		return
	}

	// Nezha 服务器 → 批量导入规格。只映射台账字段；
	// 指标、Agent 版本等由 Rollcall 自己的 Agent 重新采集/上报。
	specs := make([]importNodeSpec, 0, len(payload.Result))
	for _, ns := range payload.Result {
		name := strings.TrimSpace(ns.Name)
		if name == "" {
			continue
		}
		spec := importNodeSpec{Name: name}
		if note := sanitize.Text(strings.TrimSpace(ns.Note), 200); note != "" {
			spec.Note = note
		}
		if cc := strings.ToUpper(nezhaCountry(ns.Host)); len(cc) == 2 {
			spec.Country = cc
		}
		specs = append(specs, spec)
	}
	if len(specs) == 0 {
		fail(w, http.StatusBadGateway, "Nezha 清单里没有可导入的服务器（名字全为空）")
		return
	}

	created, skipped := s.applyNodeImport(r, specs)
	writeJSON(w, http.StatusOK, map[string]any{
		"created": created,
		"skipped": skipped,
	})
}

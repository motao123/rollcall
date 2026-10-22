package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestReportRecordsSourceIP 覆盖「最近上报来源 IP」：
// 只记通过鉴权的那一次，IP 变化要跟着变，且绝不能出现在公开接口里。
func TestReportRecordsSourceIP(t *testing.T) {
	h := newHealthServer(t)
	const tok = "health-test-admin-token-123456"

	create := httptest.NewRequest(http.MethodPost, "/api/admin/nodes",
		strings.NewReader(`{"name":"ip-node","provider":"test"}`))
	create.Header.Set("Content-Type", "application/json")
	create.Header.Set("X-Admin-Token", tok)
	cw := httptest.NewRecorder()
	h.ServeHTTP(cw, create)
	if cw.Code != http.StatusCreated {
		t.Fatalf("建节点失败: %d %s", cw.Code, cw.Body.String())
	}
	var node struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(cw.Body.Bytes(), &node); err != nil || node.Token == "" {
		t.Fatalf("建节点响应缺少令牌: %s", cw.Body.String())
	}

	report := func(remoteAddr string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/report",
			strings.NewReader(`{"nodeId":"`+node.ID+`","host":{"hostname":"ip-node"},"metrics":{"cpuPct":1}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Node-Token", node.Token)
		req.RemoteAddr = remoteAddr
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("上报失败: %d %s", w.Code, w.Body.String())
		}
	}

	lastIP := func() string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/admin/nodes/"+node.ID, nil)
		req.Header.Set("X-Admin-Token", tok)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("读取节点失败: %d %s", w.Code, w.Body.String())
		}
		var got struct {
			LastIP string `json:"lastIP"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("解析节点响应: %v", err)
		}
		return got.LastIP
	}

	report("203.0.113.9:51234")
	if ip := lastIP(); ip != "203.0.113.9" {
		t.Fatalf("首次上报应记录来源 IP，实际 %q", ip)
	}

	// 换地址（例如迁移或换线路）后必须跟着变
	report("198.51.100.7:41000")
	if ip := lastIP(); ip != "198.51.100.7" {
		t.Fatalf("来源变化后应更新，实际 %q", ip)
	}

	// 公开接口绝不带出这个字段（IP 属于资产信息，公开页只承诺脱敏视图）
	pub := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	pw := httptest.NewRecorder()
	h.ServeHTTP(pw, pub)
	if pw.Code != http.StatusOK {
		t.Fatalf("公开状态接口应为 200，实际 %d", pw.Code)
	}
	body := pw.Body.String()
	if strings.Contains(body, "lastIP") || strings.Contains(body, "198.51.100.7") {
		t.Fatalf("公开接口泄漏了上报 IP: %s", body)
	}
}

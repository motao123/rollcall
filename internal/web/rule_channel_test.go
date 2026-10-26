package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// seedChannels 往 settings 里写一份含两个渠道的通知配置。
func seedChannels(t *testing.T, st interface {
	SetSetting(string, string)
}) {
	t.Helper()
	cfg := `{"enabled":true,"minSeverity":"info","channels":[` +
		`{"id":"ch_ops","type":"webhook","name":"值班群","enabled":true,"options":{"url":"http://127.0.0.1:1/a"}},` +
		`{"id":"ch_audit","type":"webhook","name":"审计群","enabled":true,"options":{"url":"http://127.0.0.1:1/b"}}]}`
	st.SetSetting("notify", cfg)
}

func postJSON(t *testing.T, h http.Handler, tok, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin-Token", tok)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestRuleAcceptsKnownChannels 覆盖"规则限定渠道"的写入路径。
//
// 这一层必须测：notify 自己过滤得再对，只要规则接口把 channels 丢了，
// 线上表现就是"配了没生效"，而且不会报错。
func TestRuleAcceptsKnownChannels(t *testing.T) {
	const admin = "rule-channel-admin-123456789"
	st := publicStore(t)
	seedChannels(t, st)
	srv := New(st, nil, nil, Config{AdminToken: admin})
	h := srv.Handler()

	body := `{"name":"磁盘将满","metric":"disk","op":"gt","threshold":90,"duration":300,
	          "severity":"warn","channels":["ch_ops","ch_audit"]}`
	if w := postJSON(t, h, admin, "/api/admin/rules", body); w.Code != http.StatusCreated {
		t.Fatalf("应创建成功，实际 %d: %s", w.Code, w.Body.String())
	}

	var got []string
	for _, r := range st.Rules() {
		if r.Name == "磁盘将满" {
			got = r.Channels
		}
	}
	if len(got) != 2 || got[0] != "ch_ops" || got[1] != "ch_audit" {
		t.Fatalf("规则应保存两个渠道，实际 %v", got)
	}
}

// TestRuleRejectsUnknownChannel 确认打错的渠道 ID 在保存时就被拦住。
//
// 不拦的后果：告警发出那一刻才发现没有目标渠道，而那时配置页面早已关掉；
// 若再叠加"静默丢弃"，就是一次完全无声的漏报。
func TestRuleRejectsUnknownChannel(t *testing.T) {
	const admin = "rule-channel-admin-223456789"
	st := publicStore(t)
	seedChannels(t, st)
	srv := New(st, nil, nil, Config{AdminToken: admin})
	h := srv.Handler()

	body := `{"name":"错渠道","metric":"cpu","op":"gt","threshold":90,"severity":"warn",
	          "channels":["ch_ops","ch_typo"]}`
	w := postJSON(t, h, admin, "/api/admin/rules", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("未知渠道应 400，实际 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "ch_typo") {
		t.Errorf("错误信息应指出是哪个 ID，实际: %s", w.Body.String())
	}
	if n := len(st.Rules()); n != 0 {
		t.Errorf("被拒绝的规则不应落库，实际有 %d 条", n)
	}
}

// TestRuleMetaExposesChannelIDs 确认元数据给出渠道 ID（前端要按 ID 选），
// 且不包含 options（那里是 webhook 地址等凭据）。
func TestRuleMetaExposesChannelIDs(t *testing.T) {
	const admin = "rule-channel-admin-323456789"
	st := publicStore(t)
	seedChannels(t, st)
	srv := New(st, nil, nil, Config{AdminToken: admin})
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/admin/rule-meta", nil)
	req.Header.Set("X-Admin-Token", admin)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("元数据应 200，实际 %d", w.Code)
	}
	var meta struct {
		ChannelList []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"channelList"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &meta); err != nil {
		t.Fatalf("解析元数据: %v", err)
	}
	if len(meta.ChannelList) != 2 {
		t.Fatalf("应返回 2 个渠道，实际 %d", len(meta.ChannelList))
	}
	if meta.ChannelList[0].ID != "ch_ops" || meta.ChannelList[0].Name != "值班群" {
		t.Errorf("渠道选项不对: %+v", meta.ChannelList[0])
	}
	if strings.Contains(w.Body.String(), "127.0.0.1:1/a") {
		t.Error("元数据不能带出渠道的 options（含 webhook 地址等凭据）")
	}
}

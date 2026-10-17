package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/motao/probe/internal/model"
	"github.com/motao/probe/internal/store"
)

func TestCSVSafeBlocksFormulaInjection(t *testing.T) {
	cases := []struct{ in, want string }{
		{"=cmd|'/c calc'!A1", "'=cmd|'/c calc'!A1"},
		{"+1234", "'+1234"},
		{"-2+3", "'-2+3"},
		{"@SUM(A1)", "'@SUM(A1)"},
		{"\t=1+1", "'\t=1+1"},
		{"\r=1+1", "'\r=1+1"},
		// 正常数据零影响
		{"腾讯云-sg", "腾讯云-sg"},
		{"node-01", "node-01"},
		{"Ubuntu 22.04", "Ubuntu 22.04"},
		{"", ""},
	}
	for _, c := range cases {
		if got := csvSafe(c.in); got != c.want {
			t.Errorf("csvSafe(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

func TestSanitizeStripsHeaderInjection(t *testing.T) {
	cases := []struct{ in, want string }{
		{"正常节点名", "正常节点名"},
		{"a/b\\c:d", "a-b-c-d"},
		// CR/LF 必须被移除，否则可注入 Content-Disposition 之外的响应头
		// （冒号与空格也会被 Replacer 换成 -，故此处是 "--"）
		{"evil\r\nX-Injected: yes", "evilX-Injected--yes"},
		{"tab\tname", "tab-name"},
		{"ctrl\x00\x1fend", "ctrlend"},
	}
	for _, c := range cases {
		got := sanitize(c.in)
		if got != c.want {
			t.Errorf("sanitize(%q) = %q, 期望 %q", c.in, got, c.want)
		}
		// 无论输入如何，输出绝不能含 CR/LF
		if bytes.ContainsAny([]byte(got), "\r\n") {
			t.Errorf("sanitize(%q) 仍含 CR/LF: %q", c.in, got)
		}
	}
}

// TestNodeUpdatePreservesToken 是回归测试。
//
// 曾经的缺陷：POST /api/admin/nodes 无条件 in.Token = store.NewToken()，
// 于是带着已存在 ID 更新台账会**静默轮换令牌**，该节点 Agent 立即开始 401，
// 与之前 json:"-" 令牌持久化 bug 同类（都是重启/更新后令牌失效）。
func TestNodeUpdatePreservesToken(t *testing.T) {
	st := store.New(t.TempDir())
	const adminTok = "test-admin-token-1234567890"

	// 预置一个已有令牌的节点
	n := &model.Node{ID: "n1", Name: "节点一", Provider: "old", Token: "original-token"}
	st.UpsertNode(n)

	srv := New(st, nil, nil, Config{AdminToken: adminTok})

	// 用同一 ID 更新台账（改名/改服务商），不应触碰令牌
	body, _ := json.Marshal(model.Node{ID: "n1", Name: "节点一改", Provider: "new"})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/nodes", bytes.NewReader(body))
	req.Header.Set("X-Admin-Token", adminTok)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != 201 {
		t.Fatalf("更新节点返回 %d: %s", w.Code, w.Body.String())
	}
	var got adminNodeView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if got.Token != "original-token" {
		t.Errorf("令牌被轮换: got %q, 期望保留 %q", got.Token, "original-token")
	}
	if got.Name != "节点一改" || got.Provider != "new" {
		t.Errorf("台账字段未更新: %+v", got.Node)
	}
}

// TestNewNodeGetsToken 确认全新节点仍会生成令牌。
func TestNewNodeGetsToken(t *testing.T) {
	st := store.New(t.TempDir())
	const adminTok = "test-admin-token-1234567890"
	srv := New(st, nil, nil, Config{AdminToken: adminTok})

	body, _ := json.Marshal(model.Node{Name: "新节点"})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/nodes", bytes.NewReader(body))
	req.Header.Set("X-Admin-Token", adminTok)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != 201 {
		t.Fatalf("创建节点返回 %d", w.Code)
	}
	var got adminNodeView
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Token) < 32 {
		t.Errorf("新节点未生成有效令牌: %q", got.Token)
	}
	if got.ID == "" {
		t.Error("新节点未生成 ID")
	}
}

// TestAssetsExportEscapesHostOS 验证 Agent 自报的恶意 os 字段被中和。
func TestAssetsExportEscapesHostOS(t *testing.T) {
	st := store.New(t.TempDir())
	const adminTok = "test-admin-token-1234567890"

	st.UpsertNode(&model.Node{ID: "n1", Name: "节点一"})
	// 模拟被攻破的 Agent 自报恶意 os
	st.AddMetrics("n1", model.HostInfo{Hostname: "h", OS: "=cmd|'/c calc'!A1"}, model.Metrics{TS: 1000})

	srv := New(st, nil, nil, Config{AdminToken: adminTok})
	req := httptest.NewRequest(http.MethodGet, "/api/export/assets.csv", nil)
	req.Header.Set("X-Admin-Token", adminTok)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("导出返回 %d", w.Code)
	}
	out := w.Body.String()
	// 恶意公式必须以单引号前缀出现，不能裸 =cmd 开头
	if bytes.Contains([]byte(out), []byte("\n=cmd")) ||
		bytes.Contains([]byte(out), []byte(",=cmd")) {
		t.Errorf("CSV 公式注入未被中和:\n%s", out)
	}
	if !bytes.Contains([]byte(out), []byte("'=cmd")) {
		t.Errorf("未见转义后的安全形式:\n%s", out)
	}
}

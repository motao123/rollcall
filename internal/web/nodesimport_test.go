package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/store"
)

func importEnv(t *testing.T) (*store.Store, http.Handler) {
	t.Helper()
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "e1", Name: "已存在-香港", Currency: "USD"})
	srv := New(st, nil, nil, Config{AdminToken: "test-admin-token-1234567890"})
	return st, srv.Handler()
}

func importPost(h http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/admin/nodes/import", strings.NewReader(body))
	req.Header.Set("X-Admin-Token", "test-admin-token-1234567890")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

type importResp struct {
	Created []struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Command string `json:"command"`
	} `json:"created"`
	Skipped []struct {
		Index  int    `json:"index"`
		Name   string `json:"name"`
		Reason string `json:"reason"`
	} `json:"skipped"`
}

// TestNodesImportCreatesAndReturnsCommands 覆盖正常路径：
// 两台都建成功，响应里每台都带一行接入命令（含该节点自己的令牌）。
func TestNodesImportCreatesAndReturnsCommands(t *testing.T) {
	st, h := importEnv(t)
	body := `{"nodes":[
		{"name":"迁移-01","provider":"racknerd","region":"圣何塞","country":"US","group":"prod","price":29.9,"expireAt":"2027-06-01"},
		{"name":"迁移-02","region":"东京","country":"JP"}
	]}`
	w := importPost(h, body)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var r importResp
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Created) != 2 || len(r.Skipped) != 0 {
		t.Fatalf("应创建 2 条、跳过 0 条，实际 created=%d skipped=%d", len(r.Created), len(r.Skipped))
	}
	nodes := st.Nodes()
	if len(nodes) != 3 { // 原有 1 台 + 新增 2 台
		t.Fatalf("台账应有 3 个节点，实际 %d", len(nodes))
	}
	for _, c := range r.Created {
		n, ok := st.Node(c.ID)
		if !ok {
			t.Fatalf("节点 %s 未落库", c.ID)
		}
		if n.Token == "" {
			t.Fatalf("%s 没有生成令牌，Agent 无法上报", c.Name)
		}
		if !strings.Contains(c.Command, "/agent/install.sh") ||
			!strings.Contains(c.Command, c.ID) ||
			!strings.Contains(c.Command, n.Token) {
			t.Fatalf("接入命令不完整: %s", c.Command)
		}
		// 令牌只在命令里出现一次，不能出现别的节点令牌。
		if strings.Count(c.Command, "sh -s -- ") != 1 {
			t.Fatalf("命令格式异常: %s", c.Command)
		}
	}
}

// TestNodesImportSkipsDuplicatesAndInvalid 覆盖两类跳过：
// 与现有节点重名（含大小写/空格差异）、批内重名、缺 name。
// 关键点：跳过不能变成整批失败——迁移数据里重名是常态。
func TestNodesImportSkipsDuplicatesAndInvalid(t *testing.T) {
	st, h := importEnv(t)
	body := `{"nodes":[
		{"name":"已存在-香港"},
		{"name":"  已存在-香港  "},
		{"name":"新机器"},
		{"name":"新机器"},
		{"name":"   "},
		{"name":"坏国家","country":"USA"}
	]}`
	w := importPost(h, body)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200（部分跳过），实际 %d: %s", w.Code, w.Body.String())
	}
	var r importResp
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Created) != 1 {
		t.Fatalf("只有「新机器」应创建成功，实际 created=%d", len(r.Created))
	}
	if len(r.Skipped) != 5 {
		t.Fatalf("应有 5 条跳过，实际 %d: %+v", len(r.Skipped), r.Skipped)
	}
	if r.Skipped[0].Reason != "节点名已存在" {
		t.Errorf("重名原因不符: %q", r.Skipped[0].Reason)
	}
	if r.Skipped[1].Reason != "节点名已存在" {
		t.Errorf("带空格的重名也应判为已存在: %q", r.Skipped[1].Reason)
	}
	if r.Skipped[2].Reason != "节点名已存在" {
		t.Errorf("批内重复应被判重: %q", r.Skipped[2].Reason)
	}
	if !strings.Contains(r.Skipped[3].Reason, "name is required") {
		t.Errorf("缺 name 的原因不符: %q", r.Skipped[3].Reason)
	}
	if !strings.Contains(r.Skipped[4].Reason, "country") {
		t.Errorf("非法国家码的原因不符: %q", r.Skipped[4].Reason)
	}
	// 跳过的不应进台账：原 1 台 + 新建 1 台
	if got := len(st.Nodes()); got != 2 {
		t.Fatalf("台账应只有 2 个节点，实际 %d", got)
	}
}

// TestNodesImportRejectsBadRequests 覆盖整体拒绝的三种情况。
func TestNodesImportRejectsBadRequests(t *testing.T) {
	_, h := importEnv(t)

	if w := importPost(h, `{"nodes":[]}`); w.Code != http.StatusBadRequest {
		t.Errorf("空数组应 400，实际 %d", w.Code)
	}
	if w := importPost(h, `not-json`); w.Code != http.StatusBadRequest {
		t.Errorf("非法 JSON 应 400，实际 %d", w.Code)
	}

	// 超过上限：宁可整体拒绝，也不要往台账里塞几百个待删节点。
	var sb strings.Builder
	sb.WriteString(`{"nodes":[`)
	for i := 0; i < maxImportNodes+1; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"name":"n%d"}`, i)
	}
	sb.WriteString(`]}`)
	if w := importPost(h, sb.String()); w.Code != http.StatusBadRequest {
		t.Errorf("超上限应 400，实际 %d", w.Code)
	}

	// 未鉴权不得导入。
	req := httptest.NewRequest(http.MethodPost, "/api/admin/nodes/import",
		strings.NewReader(`{"nodes":[{"name":"x"}]}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Error("缺少管理令牌时必须拒绝")
	}
}

// TestNodesImportCommandUsesInsecureFlagOnlyForPublicHTTP 确认命令里的
// --allow-insecure 与安装脚本的闸门一致：回环明文不加，公网明文加。
func TestNodesImportCommandUsesInsecureFlagOnlyForPublicHTTP(t *testing.T) {
	srv := New(store.New(t.TempDir()), nil, nil, Config{})
	cases := []struct {
		base string
		want bool
	}{
		{"http://127.0.0.1:19080", false},
		{"http://localhost:19080", false},
		{"http://[::1]:19080", false},
		{"http://203.0.113.10:19080", true},
		{"https://203.0.113.10:19080", false},
	}
	for _, c := range cases {
		cmd := srv.agentInstallCommand(c.base, "n_1", "tok")
		got := strings.Contains(cmd, "--allow-insecure")
		if got != c.want {
			t.Errorf("%s: --allow-insecure=%v，期望 %v（%s）", c.base, got, c.want, cmd)
		}
	}
}

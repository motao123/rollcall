package web

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/motao123/rollcall/internal/backup"
	"github.com/motao123/rollcall/internal/model"
)

const scanTok = "scanfix-admin-token-123456"

func scanServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	srv := New(publicStore(t), nil, nil, Config{AdminToken: scanTok})
	return srv, srv.Handler()
}

func doReq(t *testing.T, h http.Handler, method, path, body string, admin bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if admin {
		req.Header.Set("X-Admin-Token", scanTok)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestCompareAggregatesUngroupedNodes 回归：
// 查用 grouped[n.Group]（空串）、写用 grouped["未分组"]，导致每台未分组节点
// 都新建聚合并覆盖前一个，最终只剩一台的统计。
func TestCompareAggregatesUngroupedNodes(t *testing.T) {
	srv, h := scanServer(t)
	for _, id := range []string{"n1", "n2", "n3"} {
		srv.st.UpsertNode(&model.Node{ID: id, Name: id}) // 不设 Group
	}
	w := doReq(t, h, http.MethodGet, "/api/compare", "", true)
	if w.Code != 200 {
		t.Fatalf("compare 失败: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Summary struct {
			Groups []struct {
				Group string `json:"group"`
				Nodes int    `json:"nodes"`
			} `json:"groups"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(resp.Summary.Groups) != 1 {
		t.Fatalf("未分组节点应聚合为 1 组，实际 %d 组: %+v", len(resp.Summary.Groups), resp.Summary.Groups)
	}
	g := resp.Summary.Groups[0]
	if g.Group != "未分组" || g.Nodes != 3 {
		t.Fatalf("聚合错误: %+v", g)
	}
}

// TestBatchProviderRegionAccepted 回归：
// provider/region 在校验 switch 里漏了分支，会被直接判为"未知操作"。
func TestBatchProviderRegionAccepted(t *testing.T) {
	srv, h := scanServer(t)
	srv.st.UpsertNode(&model.Node{ID: "n1", Name: "a"})

	for _, action := range []string{"provider", "region"} {
		w := doReq(t, h, http.MethodPost, "/api/admin/nodes/batch",
			`{"ids":["n1"],"action":"`+action+`","value":"v-`+action+`"}`, true)
		if w.Code != 200 {
			t.Fatalf("%s 应成功，实际 %d: %s", action, w.Code, w.Body.String())
		}
		var resp struct {
			Changed int `json:"changed"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp.Changed != 1 {
			t.Fatalf("%s 应变更 1 台，实际 %+v", action, resp)
		}
	}
	n, _ := srv.st.Node("n1")
	if n.Provider != "v-provider" || n.Region != "v-region" {
		t.Fatalf("字段未写入: provider=%q region=%q", n.Provider, n.Region)
	}

	// 超长值仍应被拒绝
	long := strings.Repeat("x", 65)
	w := doReq(t, h, http.MethodPost, "/api/admin/nodes/batch",
		`{"ids":["n1"],"action":"provider","value":"`+long+`"}`, true)
	if w.Code != 400 {
		t.Fatalf("超长 provider 应 400，实际 %d", w.Code)
	}
}

// TestStatusPagePartialUpdateKeepsSections 回归：
// 展示配置表单只提交 title/historyDays/announcement/visibility/blocks，
// PUT 若以默认值为基底解码，会把用户拖拽好的模块顺序重置回默认。
func TestStatusPagePartialUpdateKeepsSections(t *testing.T) {
	srv, h := scanServer(t)

	custom := `{"sections":["history","nodes","services","map","incidents","activity"]}`
	if w := doReq(t, h, http.MethodPut, "/api/admin/status-page", custom, true); w.Code != 200 {
		t.Fatalf("保存顺序失败: %d %s", w.Code, w.Body.String())
	}

	// 模拟表单提交：只带标题与其它常规字段，不带 sections
	partial := `{"title":"我的状态","historyDays":30,"announcement":{"enabled":false},"visibility":{"metrics":true},"blocks":{"history":true}}`
	if w := doReq(t, h, http.MethodPut, "/api/admin/status-page", partial, true); w.Code != 200 {
		t.Fatalf("部分保存失败: %d %s", w.Code, w.Body.String())
	}

	cfg := srv.st.StatusPage()
	if cfg.Sections[0] != "history" {
		t.Fatalf("部分保存后模块顺序被重置: %v", cfg.Sections)
	}
	if cfg.Title != "我的状态" || cfg.HistoryDays != 30 {
		t.Fatalf("提交的字段未生效: %+v", cfg)
	}
}

// TestRuleCreateRejectsDuplicateID 回归：POST 带已存在 ID 曾静默覆盖。
func TestRuleCreateRejectsDuplicateID(t *testing.T) {
	_, h := scanServer(t)
	body := `{"id":"r_dup","name":"规则","metric":"cpu","op":"gt","threshold":90}`
	if w := doReq(t, h, http.MethodPost, "/api/admin/rules", body, true); w.Code != 201 {
		t.Fatalf("首次创建应 201，实际 %d: %s", w.Code, w.Body.String())
	}
	if w := doReq(t, h, http.MethodPost, "/api/admin/rules", body, true); w.Code != 409 {
		t.Fatalf("重复 ID 应 409，实际 %d: %s", w.Code, w.Body.String())
	}
}

// TestRuleUpdateRequiresOpOnMetricChange 回归：换指标不带 op 会沿用旧方向。
func TestRuleUpdateRequiresOpOnMetricChange(t *testing.T) {
	_, h := scanServer(t)
	if w := doReq(t, h, http.MethodPost, "/api/admin/rules",
		`{"id":"r1","name":"CPU","metric":"cpu","op":"gt","threshold":90}`, true); w.Code != 201 {
		t.Fatalf("建规则失败: %d %s", w.Code, w.Body.String())
	}
	if w := doReq(t, h, http.MethodPut, "/api/admin/rules/r1", `{"metric":"cert_days"}`, true); w.Code != 400 {
		t.Fatalf("换指标不带 op 应 400，实际 %d: %s", w.Code, w.Body.String())
	}
	if w := doReq(t, h, http.MethodPut, "/api/admin/rules/r1", `{"metric":"cert_days","op":"lt","threshold":7}`, true); w.Code != 200 {
		t.Fatalf("带 op 更换指标应成功，实际 %d: %s", w.Code, w.Body.String())
	}
}

// TestRestoreRejectsInvalidNotify 回归：非法通知配置曾被"先落库、再警告"，
// 导致重启后通知静默停摆。
func TestRestoreRejectsInvalidNotify(t *testing.T) {
	srv, h := scanServer(t)

	raw := []byte(`{"version":1,"nodes":[],"services":[],"rules":[],"settings":{"notify":"{not-json"}}`)
	enc, err := backup.Encrypt(raw, "RestorePass123")
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	body := `{"password":"RestorePass123","confirm":true,"data":"` +
		base64.StdEncoding.EncodeToString(enc) + `"}`
	w := doReq(t, h, http.MethodPost, "/api/admin/restore", body, true)
	if w.Code != 400 {
		t.Fatalf("非法 notify 应拒绝导入，实际 %d: %s", w.Code, w.Body.String())
	}
	if got := srv.st.GetSetting("notify"); strings.Contains(got, "not-json") {
		t.Fatalf("非法配置不应落库: %q", got)
	}
}

// TestAgentInstallScriptUsesEscapedJSON 回归：脚本算好了 *_JSON 却没用，
// 配置里直接插原始值，地址或令牌含引号时会写出非法 JSON。
func TestAgentInstallScriptUsesEscapedJSON(t *testing.T) {
	_, h := scanServer(t)
	req := httptest.NewRequest(http.MethodGet, "/agent/install.sh", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("获取脚本失败: %d", w.Code)
	}
	body, _ := io.ReadAll(w.Body)
	script := string(body)
	if !strings.Contains(script, `"server": "$SERVER_JSON"`) ||
		!strings.Contains(script, `"nodeId": "$NODE_ID_JSON"`) ||
		!strings.Contains(script, `"token": "$TOKEN_JSON"`) {
		t.Fatalf("写配置时未使用转义变量")
	}
}

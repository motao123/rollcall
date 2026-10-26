package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/motao123/rollcall/internal/alert"
	"github.com/motao123/rollcall/internal/model"
)

// TestRuleMetricSwitchClosesOldAlerts 是 A2 的回归测试。
//
// 规则在「节点类 ↔ 服务类」之间切换 metric 后，旧告警的键语义整体变了
// （节点类 rule|node，服务类 rule|service）：引擎不再评估旧键，
// 那些"CPU 高"的告警既不会自愈、也不该伪装成恢复 —— 必须随修改一起关闭。
func TestRuleMetricSwitchClosesOldAlerts(t *testing.T) {
	const admin = "metric-switch-admin-123456789"
	st := publicStore(t)
	eng := alert.New(st)
	srv := New(st, nil, eng, Config{AdminToken: admin})
	h := srv.Handler()

	st.UpsertNode(&model.Node{ID: "n1", Name: "节点一"})
	body := `{"id":"r1","name":"CPU 高","metric":"cpu","op":"gt","threshold":90,
	          "duration":0,"severity":"warn"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/rules", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin-Token", admin)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("创建规则应 201，实际 %d: %s", w.Code, w.Body.String())
	}

	if _, ok := st.OpenAlert(&model.Alert{ID: "a1", RuleID: "r1", RuleName: "CPU 高",
		NodeID: "n1", NodeName: "节点一", Severity: "warn", Value: 95}); !ok {
		t.Fatal("开告警失败")
	}
	if got := len(st.ActiveAlertsByRule("r1")); got != 1 {
		t.Fatalf("应 1 条活跃告警，实际 %d", got)
	}

	// 切换指标类别：cpu（节点类）→ service_down（服务类）
	put := httptest.NewRequest(http.MethodPut, "/api/admin/rules/r1",
		strings.NewReader(`{"metric":"service_down","op":"gt"}`))
	put.Header.Set("Content-Type", "application/json")
	put.Header.Set("X-Admin-Token", admin)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, put)
	if w2.Code != http.StatusOK {
		t.Fatalf("更新规则应 200，实际 %d: %s", w2.Code, w2.Body.String())
	}

	if got := len(st.ActiveAlertsByRule("r1")); got != 0 {
		t.Fatalf("切换 metric 后旧告警应被关闭，实际还有 %d 条活跃", got)
	}
	for _, a := range st.Alerts(10) {
		if a.ID == "a1" && a.ResolvedAt == 0 {
			t.Fatal("原告警应标记恢复时间")
		}
	}

	// 不切换 metric 的普通更新（改阈值）不应动告警 —— 否则调一次阈值
	// 就把所有在燃的告警静默关掉，比不修还糟。
	body2 := `{"id":"r2","name":"内存高","metric":"mem","op":"gt","threshold":90,
	          "duration":0,"severity":"warn"}`
	post := httptest.NewRequest(http.MethodPost, "/api/admin/rules", strings.NewReader(body2))
	post.Header.Set("Content-Type", "application/json")
	post.Header.Set("X-Admin-Token", admin)
	h.ServeHTTP(httptest.NewRecorder(), post)
	st.UpsertNode(&model.Node{ID: "n2", Name: "节点二"})
	if _, ok := st.OpenAlert(&model.Alert{ID: "a2", RuleID: "r2", RuleName: "内存高",
		NodeID: "n2", NodeName: "节点二", Severity: "warn", Value: 95}); !ok {
		t.Fatal("开第二条告警失败")
	}
	put2 := httptest.NewRequest(http.MethodPut, "/api/admin/rules/r2",
		strings.NewReader(`{"threshold":85}`))
	put2.Header.Set("Content-Type", "application/json")
	put2.Header.Set("X-Admin-Token", admin)
	h.ServeHTTP(httptest.NewRecorder(), put2)
	if got := len(st.ActiveAlertsByRule("r2")); got != 1 {
		t.Fatalf("仅改阈值不应关闭告警，实际剩 %d 条", got)
	}
}

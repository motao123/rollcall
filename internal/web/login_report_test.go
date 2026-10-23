package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// postWithLogins 发一份带登录事件的上报。
func postWithLogins(h http.Handler, logins string) *httptest.ResponseRecorder {
	body := fmt.Sprintf(`{"nodeId":"n1","host":{"hostname":"a"},"metrics":{"cpuPct":1},"logins":%s}`, logins)
	req := httptest.NewRequest(http.MethodPost, "/api/report", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Node-Token", sigTestToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestReportRecordsLogin 上报里的登录事件要落到台账，并能被面板读到。
func TestReportRecordsLogin(t *testing.T) {
	h, st := sigEnv(t)
	ts := time.Now().Unix()
	if w := postWithLogins(h, fmt.Sprintf(
		`[{"ts":%d,"user":"root","host":"203.0.113.9","line":"pts/0"}]`, ts)); w.Code != http.StatusOK {
		t.Fatalf("上报应 200，实际 %d %s", w.Code, w.Body.String())
	}

	n, _ := st.Node("n1")
	if n.LastLogin == nil {
		t.Fatal("最近登录没有写入台账")
	}
	if n.LastLogin.User != "root" || n.LastLogin.Host != "203.0.113.9" ||
		n.LastLogin.Line != "pts/0" || n.LastLogin.TS != ts {
		t.Fatalf("登录事件字段不符: %+v", *n.LastLogin)
	}
}

// TestReportLoginDeduplicates 同一条登录被重复上报时不应反复更新（否则会重复通知）。
func TestReportLoginDeduplicates(t *testing.T) {
	h, st := sigEnv(t)
	ts := time.Now().Unix() - 10
	payload := fmt.Sprintf(`[{"ts":%d,"user":"root","host":"203.0.113.9","line":"pts/0"}]`, ts)

	for i := 0; i < 3; i++ {
		if w := postWithLogins(h, payload); w.Code != http.StatusOK {
			t.Fatalf("第 %d 次上报应 200，实际 %d", i+1, w.Code)
		}
	}
	n, _ := st.Node("n1")
	if n.LastLogin == nil || n.LastLogin.TS != ts {
		t.Fatalf("重复上报后状态应保持不变: %+v", n.LastLogin)
	}

	// 更旧的事件不该把最新登录回退
	older := fmt.Sprintf(`[{"ts":%d,"user":"old","host":"198.51.100.1","line":"pts/9"}]`, ts-100)
	if w := postWithLogins(h, older); w.Code != http.StatusOK {
		t.Fatalf("上报应 200，实际 %d", w.Code)
	}
	n, _ = st.Node("n1")
	if n.LastLogin.User != "root" {
		t.Fatalf("旧事件不该覆盖最新登录，实际 %+v", *n.LastLogin)
	}
}

// TestReportLoginRejectsFuture 未来时间戳必须丢弃。
//
// 一条 +10 年的登录会让 LastLogin 永久"最新"，之后所有真实登录都被判为旧事件、
// 不再通知 —— 等于把告警静默掉。Agent 时钟错乱或伪造都能触发。
func TestReportLoginRejectsFuture(t *testing.T) {
	h, st := sigEnv(t)
	future := time.Now().Add(10 * 365 * 24 * time.Hour).Unix()
	if w := postWithLogins(h, fmt.Sprintf(
		`[{"ts":%d,"user":"attacker","host":"1.2.3.4","line":"pts/1"}]`, future)); w.Code != http.StatusOK {
		t.Fatalf("上报本身应 200（跳过该事件而不是拒整份），实际 %d", w.Code)
	}
	n, _ := st.Node("n1")
	if n.LastLogin != nil {
		t.Fatalf("未来时间戳的登录应被丢弃，实际 %+v", *n.LastLogin)
	}
}

// TestReportLoginSanitized 用户名/来源来自登录者，必须清洗后再入库。
func TestReportLoginSanitized(t *testing.T) {
	h, st := sigEnv(t)
	ts := time.Now().Unix() - 5
	evil := strings.Repeat("A", 200)
	if w := postWithLogins(h, fmt.Sprintf(
		`[{"ts":%d,"user":"%s","host":"%s","line":"pts/0"}]`, ts, evil, evil)); w.Code != http.StatusOK {
		t.Fatalf("上报应 200，实际 %d", w.Code)
	}
	n, _ := st.Node("n1")
	if n.LastLogin == nil {
		t.Fatal("超长用户名不该导致整条丢弃")
	}
	if len(n.LastLogin.User) > 32 || len(n.LastLogin.Host) > 64 {
		t.Fatalf("字段未被截断: user=%d host=%d", len(n.LastLogin.User), len(n.LastLogin.Host))
	}
}

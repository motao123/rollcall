package web

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/agentwire"
	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/store"
)

const sigTestToken = "sig-node-token-0123456789abcdef"

// sigEnv 建一个只有一台节点、公开页无关的最小环境。
func sigEnv(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	st := store.New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "n1", Name: "机-A", Token: sigTestToken})
	return New(st, nil, nil, Config{AdminToken: "sig-admin-token-1234567890"}).Handler(), st
}

func reportBody() string {
	return `{"nodeId":"n1","host":{"hostname":"a"},"metrics":{"cpuPct":1}}`
}

// postReport 发一份上报。sig 为空表示不带签名头。
func postReport(h http.Handler, ts int64, nonce, sig, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/report", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Node-Token", sigTestToken)
	if sig != "" {
		req.Header.Set(agentwire.HeaderNodeTS, strconv.FormatInt(ts, 10))
		req.Header.Set(agentwire.HeaderNodeNonce, nonce)
		req.Header.Set(agentwire.HeaderNodeSig, sig)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestReportSignatureAccepted 正确签名 → 200，并记录 signed 与时钟偏差。
func TestReportSignatureAccepted(t *testing.T) {
	h, st := sigEnv(t)
	body := reportBody()
	ts := time.Now().Add(30 * time.Second).Unix() // 故意偏 30 秒，验证偏差被记录
	sig := agentwire.ReportSignature(sigTestToken, ts, "nonce-1", []byte(body))

	if w := postReport(h, ts, "nonce-1", sig, body); w.Code != http.StatusOK {
		t.Fatalf("正确签名应 200，实际 %d %s", w.Code, w.Body.String())
	}
	n, _ := st.Node("n1")
	if !n.Signed {
		t.Fatal("接受签名上报后 Signed 应为 true")
	}
	if n.ClockSkew < 25 || n.ClockSkew > 35 {
		t.Fatalf("时钟偏差应约 30 秒，实际 %d", n.ClockSkew)
	}
}

// TestReportSignatureRejects 覆盖三类拒绝：体被改、超窗、重放。
func TestReportSignatureRejects(t *testing.T) {
	h, st := sigEnv(t)
	body := reportBody()
	now := time.Now().Unix()

	// 1) 签名对不上（body 被改）
	sig := agentwire.ReportSignature(sigTestToken, now, "nonce-a", []byte(body))
	if w := postReport(h, now, "nonce-a", sig, body+""); w.Code != http.StatusOK {
		t.Fatalf("原样上报应 200，实际 %d", w.Code)
	}
	// 换一份 body、沿用同一签名
	sig2 := agentwire.ReportSignature(sigTestToken, now, "nonce-b", []byte(body))
	tampered := `{"nodeId":"n1","host":{"hostname":"a"},"metrics":{"cpuPct":99}}`
	if w := postReport(h, now, "nonce-b", sig2, tampered); w.Code != http.StatusUnauthorized {
		t.Fatalf("body 被改应 401，实际 %d %s", w.Code, w.Body.String())
	}

	// 2) 时间戳超窗（重放的方向）
	far := now + agentwire.SigWindow + 60
	sig3 := agentwire.ReportSignature(sigTestToken, far, "nonce-c", []byte(body))
	w := postReport(h, far, "nonce-c", sig3, body)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("超窗应 401，实际 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "时钟") {
		t.Fatalf("超窗的拒绝理由应提示检查时钟，实际 %s", w.Body.String())
	}

	// 3) 重放：同一 ts/nonce/body 再来一次
	sig4 := agentwire.ReportSignature(sigTestToken, now, "nonce-d", []byte(body))
	if w := postReport(h, now, "nonce-d", sig4, body); w.Code != http.StatusOK {
		t.Fatalf("首次应 200，实际 %d", w.Code)
	}
	w = postReport(h, now, "nonce-d", sig4, body)
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "nonce") {
		t.Fatalf("重放应因 nonce 重复被拒，实际 %d %s", w.Code, w.Body.String())
	}

	// 全程未签名状态不该被这些拒绝污染
	n, _ := st.Node("n1")
	if n.ClockSkew != 0 {
		t.Fatalf("拒绝的上报不该写入时钟偏差，实际 %d", n.ClockSkew)
	}
}

// TestReportSignatureOptionalByDefault 默认不强制：不带签名照旧 200。
// 这条是升级安全的关键——服务端先升级时，旧 Agent 不能因此失联。
func TestReportSignatureOptionalByDefault(t *testing.T) {
	h, st := sigEnv(t)
	if w := postReport(h, 0, "", "", reportBody()); w.Code != http.StatusOK {
		t.Fatalf("未签名上报在默认策略下应 200，实际 %d %s", w.Code, w.Body.String())
	}
	n, _ := st.Node("n1")
	if n.Signed {
		t.Fatal("未签名的上报不该被标成 Signed")
	}
}

// TestReportSignatureRequired 打开开关后：未签名 401，签名 200。
func TestReportSignatureRequired(t *testing.T) {
	h, st := sigEnv(t)
	const admin = "sig-admin-token-1234567890"

	put := httptest.NewRequest(http.MethodPut, "/api/admin/report-security",
		strings.NewReader(`{"requireSignature":true}`))
	put.Header.Set("X-Admin-Token", admin)
	put.Header.Set("Content-Type", "application/json")
	pw := httptest.NewRecorder()
	h.ServeHTTP(pw, put)
	if pw.Code != http.StatusOK {
		t.Fatalf("打开开关失败: %d %s", pw.Code, pw.Body.String())
	}

	w := postReport(h, 0, "", "", reportBody())
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("强制签名时未签名应 401，实际 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "v1.16.0") {
		t.Fatalf("拒绝理由应指出该怎么办，实际 %s", w.Body.String())
	}

	body := reportBody()
	ts := time.Now().Unix()
	sig := agentwire.ReportSignature(sigTestToken, ts, "nonce-z", []byte(body))
	if w := postReport(h, ts, "nonce-z", sig, body); w.Code != http.StatusOK {
		t.Fatalf("签名上报应 200，实际 %d %s", w.Code, w.Body.String())
	}
	if n, _ := st.Node("n1"); !n.Signed {
		t.Fatal("签名通过后 Signed 应为 true")
	}
}

// TestReportSecurityListsUnsigned 管理接口要把"还没签名的在线节点"列出来，
// 否则没人知道打开开关会让谁失联。
func TestReportSecurityListsUnsigned(t *testing.T) {
	h, st := sigEnv(t)
	const admin = "sig-admin-token-1234567890"
	// 制造一台"在线但未签名"的节点：上报一次即可
	if w := postReport(h, 0, "", "", reportBody()); w.Code != http.StatusOK {
		t.Fatalf("上报失败: %d", w.Code)
	}
	// 再造一台"从未上报"的节点，它不该出现在名单里
	st.UpsertNode(&model.Node{ID: "n2", Name: "机-离线", Token: "another-token-0123456789"})

	req := httptest.NewRequest(http.MethodGet, "/api/admin/report-security", nil)
	req.Header.Set("X-Admin-Token", admin)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("读取策略失败: %d %s", w.Code, w.Body.String())
	}
	got := w.Body.String()
	if !strings.Contains(got, "机-A") {
		t.Fatalf("在线未签名节点应在名单里，实际 %s", got)
	}
	if strings.Contains(got, "机-离线") {
		t.Fatalf("从未上报的节点不该在名单里，实际 %s", got)
	}
}

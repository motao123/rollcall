package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/motao123/rollcall/internal/model"
)

// TestNormalizePanelLinkRejectsShellChars 是 v1.35.2 安全回归：
//
// url.Parse 会放行 Host 里的 `;`、`$`、`()`（实测 https://a.com$(id) 解析为
// Host="a.com$(id)"），而这个地址会被拼进「节点接入命令」由管理员复制到
// 目标服务器的 shell 里执行 —— Host 必须过字符白名单。
func TestNormalizePanelLinkRejectsShellChars(t *testing.T) {
	bad := []string{
		"https://a.com$(id)",
		"https://a.com;id",
		"https://a.com$(touch /tmp/x)",
		"http://a.com|x",
		"https://a.com b.com", // 空格：url.Parse 本身就拒绝
	}
	for _, in := range bad {
		if _, msg := normalizePanelLink(in); msg == "" {
			t.Errorf("%q 应被拒绝，实际通过", in)
		}
	}
	good := []string{
		"https://status.endgo.top",
		"https://panel.example.com:19080",
		"http://127.0.0.1:19080",
		"https://[::1]:19080",
		"https://my-panel.example-hoster.io",
	}
	for _, in := range good {
		got, msg := normalizePanelLink(in)
		if msg != "" {
			t.Errorf("%q 应通过，实际被拒：%s", in, msg)
			continue
		}
		if !safePanelHost(strings.TrimPrefix(strings.TrimPrefix(got, "https://"), "http://")) {
			t.Errorf("%q 规范化后的主机 %q 未通过白名单", in, got)
		}
	}
}

// TestAgentInstallCommandQuotesArgs 确认接入命令的插值参数都被单引号包裹：
// 即使上游校验被绕过（或未来新增调用方），拼接侧也不信任参数 ——
// 单引号内 $()、;、空格全部成为字面量。
func TestAgentInstallCommandQuotesArgs(t *testing.T) {
	srv := &Server{} // agentInstallCommand 不读任何字段；避免 New(nil) 在 GetSetting 上空指针
	cmd := srv.agentInstallCommand("https://a.com$(evil)", "n_1; rm -rf /", "tok$(id)")
	for _, want := range []string{
		`'https://a.com$(evil)/agent/install.sh'`,
		`'https://a.com$(evil)'`,
		`'n_1; rm -rf /'`,
		`'tok$(id)'`,
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("命令缺少带引号的参数 %s\n实际: %s", want, cmd)
		}
	}
}

// TestPublicTokenRateLimit 是 L1 回归：
// 同一来源对 ?t= 连续猜错 tokenFailMax 次后，即使下一次拿对正确令牌
// 也必须被拒绝（窗口内封禁），证明在线枚举被限速；换一个来源不受影响。
func TestPublicTokenRateLimit(t *testing.T) {
	const tok = "test-token-0123456789abcdef"
	h, _ := gateEnv(t, func(cfg *model.StatusPage) {
		on := true
		cfg.Enabled = &on
		cfg.Token = tok
	})

	attempt := func(ip, token string) int {
		req := httptest.NewRequest(http.MethodGet, "/status?t="+token, nil)
		req.RemoteAddr = ip + ":1234"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}

	const ip = "203.0.113.77"
	for i := 0; i < tokenFailMax; i++ {
		if code := attempt(ip, "wrong-wrong-wrong"); code != http.StatusNotFound {
			t.Fatalf("第 %d 次错 token 应 404，实际 %d", i+1, code)
		}
	}
	if code := attempt(ip, tok); code != http.StatusNotFound {
		t.Fatalf("封禁期内正确令牌也应 404，实际 %d", code)
	}
	if code := attempt("198.51.100.9", tok); code != http.StatusOK {
		t.Fatalf("其他来源持正确令牌应 200，实际 %d", code)
	}
}

// TestPublicCookieSecureBehindProxy 是 L2 回归：
// TLS 由反代终止时 r.TLS 为 nil，公开 Cookie 的 Secure 必须改由
// 可信代理的 X-Forwarded-Proto 判定 —— 否则 HTTPS 部署下公开页
// Cookie 永远不带 Secure。
func TestPublicCookieSecureBehindProxy(t *testing.T) {
	const tok = "test-token-0123456789abcdef"
	h, _ := gateEnv(t, func(cfg *model.StatusPage) {
		on := true
		cfg.Enabled = &on
		cfg.Token = tok
	})

	req := httptest.NewRequest(http.MethodGet, "/status?t="+tok, nil)
	req.RemoteAddr = "127.0.0.1:1234" // 可信代理（默认仅回环）
	req.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	found := false
	for _, c := range w.Result().Cookies() {
		if c.Name != publicStatusCookie {
			continue
		}
		found = true
		if !c.Secure {
			t.Error("经可信代理的 HTTPS 请求种下的公开 Cookie 应带 Secure")
		}
	}
	if !found {
		t.Fatalf("未种下公开 Cookie（Set-Cookie: %v）", w.Header().Values("Set-Cookie"))
	}
}

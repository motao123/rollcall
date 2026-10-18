package notify

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCheckNotifyURLErrorRedactsSecret 防止 url.Parse 的错误串把凭据带进响应。
//
// *url.Error 的文本包含完整 URL；Telegram 的 bot token 在路径里、
// 通用 Webhook 的密钥常写在查询串，原样回显即泄露。
func TestCheckNotifyURLErrorRedactsSecret(t *testing.T) {
	raw := "http://telegram.example/bot123456:SECRET-TOKEN/send%zz"
	err := checkNotifyURL(raw)
	if err == nil {
		t.Fatal("非法 URL 应报错")
	}
	if strings.Contains(err.Error(), "SECRET-TOKEN") || strings.Contains(err.Error(), raw) {
		t.Fatalf("错误信息泄露了凭据: %v", err)
	}
}

// TestMigrateLegacyEmailWithoutPort 旧配置省略端口时不能迁移成字符串 "0"。
//
// "0" 非空，会让 sendEmail 进入端口校验分支并报「SMTP 端口无效」，
// 从而绕过 465 默认值，迁移后的邮件渠道全部失效。
func TestMigrateLegacyEmailWithoutPort(t *testing.T) {
	cfg, err := Parse(`{
		"enabled": true,
		"email": {
			"enabled": true,
			"smtpHost": "smtp.example.com",
			"username": "sender@example.com",
			"password": "p",
			"from": "sender@example.com",
			"to": ["rcpt@example.com"]
		}
	}`)
	if err != nil {
		t.Fatalf("迁移旧配置失败: %v", err)
	}
	var opts map[string]string
	for _, ch := range cfg.Channels {
		if ch.Type == "email" {
			opts = ch.Options
		}
	}
	if opts == nil {
		t.Fatal("未迁移出邮件通道")
	}
	if v, has := opts["smtpPort"]; has {
		t.Fatalf("缺省端口不应写入 smtpPort，实际为 %q", v)
	}

	// 显式配置端口时仍应保留。
	cfg, err = Parse(`{"email":{"smtpHost":"smtp.example.com","smtpPort":587,"from":"a@example.com"}}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range cfg.Channels {
		if ch.Type == "email" && ch.Options["smtpPort"] != "587" {
			t.Fatalf("显式端口应保留，实际 %q", ch.Options["smtpPort"])
		}
	}
}

// TestGotifyUsesGotifyKeyHeader 防止认证头写错导致每次推送都 401。
//
// Gotify 的推送接口接受 X-Gotify-Key 头（或 token 查询参数），
// 不接受 Authorization: Bearer。
func TestGotifyUsesGotifyKeyHeader(t *testing.T) {
	SetAllowPrivateTargets(true)
	t.Cleanup(func() { SetAllowPrivateTargets(false) })

	var gotKey, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Gotify-Key")
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := sendGotify(map[string]string{"serverUrl": srv.URL, "token": "app-token"}, "标题", "正文"); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if gotKey != "app-token" {
		t.Fatalf("X-Gotify-Key = %q，期望 app-token", gotKey)
	}
	if gotAuth != "" {
		t.Fatalf("不应发送 Authorization 头，实际 %q", gotAuth)
	}
}

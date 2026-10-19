package notify

import (
	"strings"
	"testing"
)

// TestMaskHidesShortSecrets 是回归测试。
//
// 旧实现对长度 > 10 的密钥保留首尾各 4 位：16 位 SMTP 授权码会以
// "abcd********wxyz" 的形式下发到浏览器，等于泄露一半凭据 ——
// 页面被截图或被浏览器插件读取即构成泄露。
func TestMaskHidesShortSecrets(t *testing.T) {
	for _, s := range []string{"abcdefghijklmnop", "12345678901", "abcdefghijklmnopqrst"} {
		if got := mask(s); got != "********" {
			t.Errorf("短密钥 %q 应整体隐藏，实际 %q", s, got)
		}
	}
	long := "0123456789abcdef0123456789abcdef"
	got := mask(long)
	if !strings.HasPrefix(got, "0123") || !strings.HasSuffix(got, "cdef") {
		t.Fatalf("长密钥的脱敏形式异常: %q", got)
	}
	if !isMasked(got) {
		t.Fatal("脱敏结果必须能被 isMasked 识别")
	}
}

// TestRedactedMasksURLCredentials 防止 webhook/serverUrl 里的令牌明文下发前端。
// Slack/DingTalk 的 webhook 地址本身就是凭据，Bark/Gotify 的自建地址也可能带查询令牌。
func TestRedactedMasksURLCredentials(t *testing.T) {
	cfg := &Config{Channels: []ChannelConfig{
		{ID: "c1", Type: "webhook", Enabled: true, Options: map[string]string{
			"url": "https://hooks.example.com/services/T000/B000/SECRETTOKEN",
		}},
		{ID: "c2", Type: "bark", Enabled: true, Options: map[string]string{
			"serverUrl": "https://bark.example.com?token=SECRETTOKEN", "deviceKey": "abcdefghijklmnop",
		}},
	}}
	red := cfg.Redacted()
	for _, ch := range red.Channels {
		for k, v := range ch.Options {
			if strings.Contains(v, "SECRETTOKEN") {
				t.Fatalf("通道 %s 的 %s 明文泄露: %q", ch.ID, k, v)
			}
		}
	}
	// 掩码必须能还原真实值，否则编辑保存会把地址清空。
	red.MergeSecrets(cfg)
	if got := red.Channels[0].Options["url"]; got != cfg.Channels[0].Options["url"] {
		t.Fatalf("地址掩码未还原: %q", got)
	}
	if got := red.Channels[1].Options["serverUrl"]; got != cfg.Channels[1].Options["serverUrl"] {
		t.Fatalf("serverUrl 掩码未还原: %q", got)
	}
}

// TestMergeSecretsDropsOrphanPlaceholder 是回归测试。
//
// 前端回传掩码占位符但找不到可复用旧值时（通道 ID 变了、类型变了、
// 或者用户从别的页面粘贴），旧实现静默保留占位符并落库：
// 之后每次投递都拿 "abcd********wxyz" 当真实密钥去请求，
// 报的是「认证失败」，排查方向完全被带偏。
func TestMergeSecretsDropsOrphanPlaceholder(t *testing.T) {
	old := &Config{Channels: []ChannelConfig{
		{ID: "c1", Type: "webhook", Options: map[string]string{"url": "https://example.com/hook", "secret": "real-secret-value-1234567890"}},
	}}

	// 1) 同 ID 同类型：还原真实值
	c := &Config{Channels: []ChannelConfig{
		{ID: "c1", Type: "webhook", Options: map[string]string{"url": "https://example.com/hook", "secret": mask("real-secret-value-1234567890")}},
	}}
	c.MergeSecrets(old)
	if got := c.Channels[0].Options["secret"]; got != "real-secret-value-1234567890" {
		t.Fatalf("应还原真实密钥，实际 %q", got)
	}

	// 2) 类型变了：占位符必须被清掉，不能落库
	c = &Config{Channels: []ChannelConfig{
		{ID: "c1", Type: "bark", Options: map[string]string{"deviceKey": "********"}},
	}}
	c.MergeSecrets(old)
	if got, ok := c.Channels[0].Options["deviceKey"]; ok {
		t.Fatalf("类型不匹配时占位符应被清除，实际保留 %q", got)
	}

	// 3) 全新通道（旧配置里没有）：同样清掉
	c = &Config{Channels: []ChannelConfig{
		{ID: "c2", Type: "webhook", Options: map[string]string{"url": "https://example.com/h", "secret": "********"}},
	}}
	c.MergeSecrets(old)
	if got, ok := c.Channels[0].Options["secret"]; ok {
		t.Fatalf("孤立占位符应被清除，实际保留 %q", got)
	}

	// 4) old == nil（首次配置 / 旧配置损坏）：清掉占位符，让 Validate 报「缺少密钥」
	c = &Config{Channels: []ChannelConfig{
		{ID: "c3", Type: "webhook", Options: map[string]string{"secret": "********"}},
	}}
	c.MergeSecrets(nil)
	if got, ok := c.Channels[0].Options["secret"]; ok {
		t.Fatalf("无旧配置时占位符应被清除，实际保留 %q", got)
	}
}

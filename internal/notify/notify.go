package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/motao/rollcall/internal/netguard"
)

// sevRank 用于 MinSeverity 过滤。
var sevRank = map[string]int{"info": 1, "warn": 2, "critical": 3}

// ChannelConfig 是一个已配置的投递通道实例。
// 同一类型可以配置多个（例如两个不同的 Telegram 群），因此用独立 ID 区分。
type ChannelConfig struct {
	ID      string            `json:"id"`
	Type    string            `json:"type"`
	Name    string            `json:"name"`
	Enabled bool              `json:"enabled"`
	Options map[string]string `json:"options"`
}

// Config 是整体通知配置，序列化后存入 store 的 settings 表。
type Config struct {
	Enabled        bool            `json:"enabled"`
	MinSeverity    string          `json:"minSeverity"`    // info | warn | critical
	RepeatInterval int             `json:"repeatInterval"` // 同一告警重复通知间隔（秒），0=不重复
	Channels       []ChannelConfig `json:"channels"`
}

// Message 是一条待投递的通知。
type Message struct {
	Title    string  `json:"title"`
	Body     string  `json:"body"`
	Severity string  `json:"severity"`
	NodeName string  `json:"nodeName"`
	NodeID   string  `json:"nodeId"`
	RuleID   string  `json:"ruleId"`
	RuleName string  `json:"ruleName"`
	AlertID  string  `json:"alertId"`
	Value    float64 `json:"value"`
	Resolved bool    `json:"resolved"`
	TS       int64   `json:"ts"`
	Link     string  `json:"link"`
}

// Result 是单通道投递结果。
type Result struct {
	Channel string `json:"channel"`
	Type    string `json:"type"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

// ---------- 解析与迁移 ----------

// legacyConfig 是 v1 的固定三通道结构，用于向后兼容迁移。
type legacyConfig struct {
	Enabled        bool   `json:"enabled"`
	MinSeverity    string `json:"minSeverity"`
	RepeatInterval int    `json:"repeatInterval"`
	Telegram       struct {
		Enabled  bool   `json:"enabled"`
		BotToken string `json:"botToken"`
		ChatID   string `json:"chatId"`
	} `json:"telegram"`
	Webhook struct {
		Enabled bool   `json:"enabled"`
		URL     string `json:"url"`
		Secret  string `json:"secret"`
	} `json:"webhook"`
	Email struct {
		Enabled  bool     `json:"enabled"`
		SMTPHost string   `json:"smtpHost"`
		SMTPPort int      `json:"smtpPort"`
		Username string   `json:"username"`
		Password string   `json:"password"`
		From     string   `json:"from"`
		To       []string `json:"to"`
	} `json:"email"`
}

// Parse 解析配置；自动把 v1 的三通道结构迁移为通用的 channels 列表。
func Parse(raw string) (*Config, error) {
	c := &Config{MinSeverity: "warn"}
	if strings.TrimSpace(raw) == "" {
		return c, nil
	}

	// 若不含 channels 键，说明是 v1 结构，走迁移
	// peek 只用于探测 JSON 结构，判断是新格式还是 v1 旧格式
	var peek map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &peek); err != nil {
		return nil, err
	}
	if _, has := peek["channels"]; !has {
		var lg legacyConfig
		if err := json.Unmarshal([]byte(raw), &lg); err != nil {
			return nil, err
		}
		c.Enabled = lg.Enabled
		c.RepeatInterval = lg.RepeatInterval
		if lg.MinSeverity != "" {
			c.MinSeverity = lg.MinSeverity
		}
		if lg.Telegram.BotToken != "" || lg.Telegram.ChatID != "" {
			c.Channels = append(c.Channels, ChannelConfig{
				ID: "m_telegram", Type: "telegram", Name: "Telegram",
				Enabled: lg.Telegram.Enabled,
				Options: map[string]string{"botToken": lg.Telegram.BotToken, "chatId": lg.Telegram.ChatID},
			})
		}
		if lg.Webhook.URL != "" {
			c.Channels = append(c.Channels, ChannelConfig{
				ID: "m_webhook", Type: "webhook", Name: "Webhook",
				Enabled: lg.Webhook.Enabled,
				Options: map[string]string{"url": lg.Webhook.URL, "secret": lg.Webhook.Secret},
			})
		}
		if lg.Email.SMTPHost != "" {
			c.Channels = append(c.Channels, ChannelConfig{
				ID: "m_email", Type: "email", Name: "邮件",
				Enabled: lg.Email.Enabled,
				Options: map[string]string{
					"smtpHost": lg.Email.SMTPHost,
					"smtpPort": strconv.Itoa(lg.Email.SMTPPort),
					"username": lg.Email.Username,
					"password": lg.Email.Password,
					"from":     lg.Email.From,
					"to":       strings.Join(lg.Email.To, ","),
				},
			})
		}
		c.normalize()
		return c, nil
	}

	if err := json.Unmarshal([]byte(raw), c); err != nil {
		return nil, err
	}
	c.normalize()
	return c, nil
}

// normalize 修正越界/非法的配置值，避免 MinSeverity 拼错后阈值失效。
func (c *Config) normalize() {
	if _, ok := sevRank[c.MinSeverity]; !ok {
		c.MinSeverity = "warn"
	}
	if c.RepeatInterval < 0 {
		c.RepeatInterval = 0
	}
	if c.RepeatInterval > 7*86400 {
		c.RepeatInterval = 7 * 86400
	}
}

// Normalize 修正越界/非法的配置值。供管理 API 直接解码后调用。
func (c *Config) Normalize() { c.normalize() }

// Validate 校验通道类型与 ID，避免保存拼错类型或重复 ID 的配置。
func (c *Config) Validate() error {
	seen := make(map[string]bool, len(c.Channels))
	for i, ch := range c.Channels {
		if _, ok := defOf(ch.Type); !ok {
			return fmt.Errorf("第 %d 个通道类型无效：%s", i+1, ch.Type)
		}
		if ch.ID != "" {
			if seen[ch.ID] {
				return fmt.Errorf("通道 ID 重复：%s", ch.ID)
			}
			seen[ch.ID] = true
		}
	}
	return nil
}

// Redacted 返回脱敏副本（敏感字段以掩码替换），用于下发前端。
func (c *Config) Redacted() *Config {
	cp := &Config{
		Enabled:        c.Enabled,
		MinSeverity:    c.MinSeverity,
		RepeatInterval: c.RepeatInterval,
		Channels:       make([]ChannelConfig, 0, len(c.Channels)),
	}
	for _, ch := range c.Channels {
		sk := secretKeys(ch.Type)
		opts := make(map[string]string, len(ch.Options))
		for k, v := range ch.Options {
			if sk[k] && v != "" {
				opts[k] = mask(v)
			} else {
				opts[k] = v
			}
		}
		cp.Channels = append(cp.Channels, ChannelConfig{
			ID: ch.ID, Type: ch.Type, Name: ch.Name, Enabled: ch.Enabled, Options: opts,
		})
	}
	return cp
}

func mask(s string) string {
	if len(s) <= 10 {
		return "********"
	}
	return s[:4] + "********" + s[len(s)-4:]
}

func isMasked(s string) bool { return strings.Contains(s, "********") }

// MergeSecrets 把前端回传的掩码占位符还原为已保存的真实值。
// 按「通道 ID + 类型」匹配，类型不一致则不复用，避免串到别的通道上。
func (c *Config) MergeSecrets(old *Config) {
	if old == nil {
		return
	}
	prev := map[string]ChannelConfig{}
	for _, ch := range old.Channels {
		prev[ch.ID] = ch
	}
	for i := range c.Channels {
		ch := &c.Channels[i]
		o, ok := prev[ch.ID]
		if !ok || o.Type != ch.Type {
			continue
		}
		sk := secretKeys(ch.Type)
		for k, v := range ch.Options {
			if sk[k] && isMasked(v) {
				if real, ok := o.Options[k]; ok {
					ch.Options[k] = real
				}
			}
		}
	}
}

// EnabledChannels 返回已启用且配置完整的通道名称。
func (c *Config) EnabledChannels() []string {
	var out []string
	for _, ch := range c.Channels {
		if !ch.Enabled {
			continue
		}
		if missingRequired(ch) != "" {
			continue
		}
		out = append(out, displayName(ch))
	}
	return out
}

func displayName(ch ChannelConfig) string {
	if ch.Name != "" {
		return ch.Name
	}
	if d, ok := defOf(ch.Type); ok {
		return d.Label
	}
	return ch.Type
}

// missingRequired 返回第一个缺失的必填字段标签，全部齐备返回空串。
func missingRequired(ch ChannelConfig) string {
	d, ok := defOf(ch.Type)
	if !ok {
		return "未知通道类型"
	}
	for _, f := range d.Fields {
		if f.Required && strings.TrimSpace(ch.Options[f.Key]) == "" {
			return f.Label
		}
	}
	// 发件人留空时回退到用户名；两者都没有则配置不完整。
	if ch.Type == "email" &&
		strings.TrimSpace(ch.Options["from"]) == "" &&
		strings.TrimSpace(ch.Options["username"]) == "" {
		return "发件人或用户名"
	}
	return ""
}

// ShouldSend 判断该严重度是否需要通知。
func (c *Config) ShouldSend(severity string) bool {
	if !c.Enabled {
		return false
	}
	min := sevRank[c.MinSeverity]
	if min == 0 {
		min = sevRank["warn"]
	}
	return sevRank[severity] >= min
}

// ---------- 投递 ----------

// allowPrivateNotify 控制通知目标是否允许访问回环/私网/CGNAT。
// 默认拒绝；自建 Gotify/Bark/Webhook 位于内网时由服务端显式开启。
var allowPrivateNotify atomic.Bool

// SetAllowPrivateTargets 设置通知通道是否允许私网目标（仅启动时调用）。
func SetAllowPrivateTargets(v bool) { allowPrivateNotify.Store(v) }

func currentNotifyPolicy() netguard.Policy {
	return netguard.Policy{AllowPrivate: allowPrivateNotify.Load()}
}

// notifyDialContext 在拨号时读取当前策略；策略可在进程启动阶段调整。
func notifyDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return currentNotifyPolicy().DialContext(ctx, network, addr)
}

// httpClient 不跟随重定向：避免鉴权头/消息内容被 3xx 带到其他主机。
var httpClient = &http.Client{
	Timeout: 12 * time.Second,
	Transport: &http.Transport{
		DialContext:       notifyDialContext,
		DisableKeepAlives: true,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// Send 向所有启用的通道投递，返回逐通道结果。
func Send(cfg *Config, m Message) []Result {
	if cfg == nil || !cfg.Enabled {
		return []Result{{Channel: "all", Type: "-", OK: false, Error: "通知未启用"}}
	}
	if !cfg.ShouldSend(m.Severity) {
		return []Result{{Channel: "all", Type: "-", OK: false,
			Error: fmt.Sprintf("严重度 %s 低于阈值 %s，已跳过", m.Severity, cfg.MinSeverity)}}
	}

	subject, body := render(m)
	var out []Result
	for _, ch := range cfg.Channels {
		if !ch.Enabled {
			continue
		}
		name := displayName(ch)
		if miss := missingRequired(ch); miss != "" {
			out = append(out, Result{Channel: name, Type: ch.Type, OK: false,
				Error: "缺少必填项：" + miss})
			continue
		}
		err := dispatch(ch, subject, body, m)
		out = append(out, Result{Channel: name, Type: ch.Type, OK: err == nil, Error: errStr(err)})
	}
	if len(out) == 0 {
		out = append(out, Result{Channel: "all", Type: "-", OK: false, Error: "没有启用的通道"})
	}
	return out
}

func errStr(e error) string {
	if e == nil {
		return ""
	}
	s := e.Error()
	// 按 rune 截断，避免把中文切成半个字符
	r := []rune(s)
	if len(r) > 120 {
		return string(r[:120]) + "…"
	}
	return s
}

// render 把消息渲染为「主题 + 正文」两段式（对齐 certimate 的 Notify(subject, message)）。
func render(m Message) (subject, body string) {
	status := "告警"
	if m.Resolved {
		status = "恢复"
	}
	subject = fmt.Sprintf("[%s] %s · %s", status, m.NodeName, m.Title)

	var b strings.Builder
	fmt.Fprintf(&b, "节点：%s\n", m.NodeName)
	fmt.Fprintf(&b, "规则：%s\n", m.RuleName)
	if m.Value != 0 {
		fmt.Fprintf(&b, "当前值：%.1f\n", m.Value)
	}
	if m.Body != "" {
		fmt.Fprintf(&b, "详情：%s\n", m.Body)
	}
	fmt.Fprintf(&b, "级别：%s\n", m.Severity)
	fmt.Fprintf(&b, "时间：%s\n", time.Unix(m.TS, 0).Format("2006-01-02 15:04:05"))
	if m.Link != "" {
		fmt.Fprintf(&b, "面板：%s\n", m.Link)
	}
	return subject, b.String()
}

func dispatch(ch ChannelConfig, subject, body string, m Message) error {
	switch ch.Type {
	case "wecom":
		return sendWecom(ch.Options, subject, body)
	case "dingtalk":
		return sendDingtalk(ch.Options, subject, body)
	case "lark":
		return sendLark(ch.Options, subject, body)
	case "telegram":
		return sendTelegram(ch.Options, subject, body)
	case "bark":
		return sendBark(ch.Options, subject, body)
	case "serverchan":
		return sendServerchan(ch.Options, subject, body)
	case "pushplus":
		return sendPushplus(ch.Options, subject, body)
	case "gotify":
		return sendGotify(ch.Options, subject, body)
	case "email":
		return sendEmail(ch.Options, subject, body)
	case "webhook":
		return sendWebhook(ch.Options, m)
	}
	return fmt.Errorf("不支持的通道类型：%s", ch.Type)
}

func postJSON(urlStr string, payload any) ([]byte, error) {
	if err := checkNotifyURL(urlStr); err != nil {
		return nil, err
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, urlStr, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "motao-rollcall/1.1")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, netErr(err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if resp.StatusCode >= 300 {
		// 不回显远端响应体：通知地址由用户配置，响应内容不可信
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return rb, nil
}

// checkNotifyURL 校验通知地址协议与地址范围。
func checkNotifyURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("通知地址无效: %w", err)
	}
	return currentNotifyPolicy().CheckURL(u)
}

// netErr 剥掉 url.Error 中携带的完整 URL。
// Telegram 的 Bot Token 在路径里，通用 Webhook 的密钥常写在查询串，
// 原样返回会把凭据带进 API 响应或日志。
func netErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}

// decodeResult 严格解析远端返回的 JSON。
// 早期实现把 Unmarshal 失败当作成功，导致配错地址也会显示「发送成功」。
func decodeResult(rb []byte, v any) error {
	if len(bytes.TrimSpace(rb)) == 0 {
		return fmt.Errorf("通知接口返回空响应")
	}
	if err := json.Unmarshal(rb, v); err != nil {
		return fmt.Errorf("通知接口响应不是合法 JSON")
	}
	return nil
}

// ---------- 企业微信 ----------

func sendWecom(o map[string]string, subject, body string) error {
	rb, err := postJSON(o["webhookUrl"], map[string]any{
		"msgtype": "text",
		"text":    map[string]string{"content": subject + "\n\n" + body},
	})
	if err != nil {
		return err
	}
	// 企业微信出错时 HTTP 仍为 200，必须解析 errcode 才能知道真实结果
	var r struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := decodeResult(rb, &r); err != nil {
		return fmt.Errorf("wecom: %w", err)
	}
	if r.ErrCode != 0 {
		return fmt.Errorf("wecom: %s (errcode %d)", r.ErrMsg, r.ErrCode)
	}
	return nil
}

// ---------- 钉钉 ----------

func sendDingtalk(o map[string]string, subject, body string) error {
	u := o["webhookUrl"]
	if secret := o["secret"]; secret != "" {
		ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
		h := hmac.New(sha256.New, []byte(secret))
		h.Write([]byte(ts + "\n" + secret))
		sign := base64.StdEncoding.EncodeToString(h.Sum(nil))
		sep := "?"
		if strings.Contains(u, "?") {
			sep = "&"
		}
		u = u + sep + "timestamp=" + ts + "&sign=" + url.QueryEscape(sign)
	}
	rb, err := postJSON(u, map[string]any{
		"msgtype": "text",
		"text":    map[string]string{"content": subject + "\n" + body},
	})
	if err != nil {
		return err
	}
	var r struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := decodeResult(rb, &r); err != nil {
		return fmt.Errorf("dingtalk: %w", err)
	}
	if r.ErrCode != 0 {
		return fmt.Errorf("dingtalk: %s (errcode %d)", r.ErrMsg, r.ErrCode)
	}
	return nil
}

// ---------- 飞书 ----------

func sendLark(o map[string]string, subject, body string) error {
	payload := map[string]any{
		"msg_type": "text",
		"content":  map[string]string{"text": subject + "\n\n" + body},
	}
	if secret := o["secret"]; secret != "" {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		h := hmac.New(sha256.New, []byte(ts+"\n"+secret))
		h.Write([]byte{})
		payload["timestamp"] = ts
		payload["sign"] = base64.StdEncoding.EncodeToString(h.Sum(nil))
	}
	rb, err := postJSON(o["webhookUrl"], payload)
	if err != nil {
		return err
	}
	var r struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := decodeResult(rb, &r); err != nil {
		return fmt.Errorf("lark: %w", err)
	}
	if r.Code != 0 {
		return fmt.Errorf("lark: %s (code %d)", r.Msg, r.Code)
	}
	return nil
}

// ---------- Telegram ----------

func sendTelegram(o map[string]string, subject, body string) error {
	u := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", o["botToken"])
	rb, err := postJSON(u, map[string]any{
		"chat_id":                  o["chatId"],
		"text":                     subject + "\n\n" + body,
		"disable_web_page_preview": true,
	})
	if err != nil {
		return err
	}
	var r struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := decodeResult(rb, &r); err != nil {
		return fmt.Errorf("telegram: %w", err)
	}
	if !r.OK {
		return fmt.Errorf("telegram: %s", r.Description)
	}
	return nil
}

// ---------- Bark ----------

func sendBark(o map[string]string, subject, body string) error {
	server := strings.TrimSpace(o["serverUrl"])
	if server == "" {
		server = "https://api.day.app/"
	}
	if !strings.HasSuffix(server, "/") {
		server += "/"
	}
	payload := map[string]any{"title": subject, "body": body}
	if k := o["deviceKey"]; k != "" {
		payload["device_key"] = k
	}
	if g := o["group"]; g != "" {
		payload["group"] = g
	}
	rb, err := postJSON(server, payload)
	if err != nil {
		return err
	}
	var r struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := decodeResult(rb, &r); err != nil {
		return fmt.Errorf("bark: %w", err)
	}
	if r.Code != 0 && r.Code != 200 {
		return fmt.Errorf("bark: %s (code %d)", r.Message, r.Code)
	}
	return nil
}

// ---------- Server酱 ----------

func sendServerchan(o map[string]string, subject, body string) error {
	key := strings.TrimSpace(o["sendKey"])
	// sendKey 只接受令牌本身；曾经允许填完整 URL，等于绕过了地址策略
	if key == "" || strings.ContainsAny(key, "/\\?#") {
		return fmt.Errorf("serverchan: sendKey 格式无效")
	}
	u := "https://sctapi.ftqq.com/" + key + ".send"
	rb, err := postJSON(u, map[string]any{"title": subject, "desp": body})
	if err != nil {
		return err
	}
	var r struct {
		Code int    `json:"code"`
		Msg  string `json:"message"`
	}
	if err := decodeResult(rb, &r); err != nil {
		return fmt.Errorf("serverchan: %w", err)
	}
	if r.Code != 0 {
		return fmt.Errorf("serverchan: %s (code %d)", r.Msg, r.Code)
	}
	return nil
}

// ---------- PushPlus ----------

func sendPushplus(o map[string]string, subject, body string) error {
	payload := map[string]any{
		"token":    o["token"],
		"title":    subject,
		"content":  body,
		"template": "txt",
	}
	if t := o["topic"]; t != "" {
		payload["topic"] = t
	}
	rb, err := postJSON("https://www.pushplus.plus/send", payload)
	if err != nil {
		return err
	}
	var r struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := decodeResult(rb, &r); err != nil {
		return fmt.Errorf("pushplus: %w", err)
	}
	if r.Code != 200 {
		return fmt.Errorf("pushplus: %s (code %d)", r.Msg, r.Code)
	}
	return nil
}

// ---------- Gotify ----------

func sendGotify(o map[string]string, subject, body string) error {
	server := strings.TrimRight(strings.TrimSpace(o["serverUrl"]), "/")
	prio := 5
	if v := strings.TrimSpace(o["priority"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			prio = n
		}
	}
	b, _ := json.Marshal(map[string]any{
		"title": subject, "message": body, "priority": prio,
	})
	u := server + "/message"
	if err := checkNotifyURL(u); err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o["token"])
	resp, err := httpClient.Do(req)
	if err != nil {
		return netErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// ---------- 通用 Webhook ----------

func sendWebhook(o map[string]string, m Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	u := strings.TrimSpace(o["url"])
	if err := checkNotifyURL(u); err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "motao-rollcall/1.1")
	if s := o["secret"]; s != "" {
		req.Header.Set("X-Rollcall-Secret", s)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return netErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// ---------- 邮件 ----------

func sendEmail(o map[string]string, subject, body string) error {
	host := strings.TrimSpace(o["smtpHost"])
	if host == "" || strings.ContainsAny(host, " \r\n\t/") {
		return fmt.Errorf("SMTP 主机无效")
	}
	port := 465
	if v := strings.TrimSpace(o["smtpPort"]); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("SMTP 端口无效")
		}
		port = n
	}
	rawFrom := strings.TrimSpace(o["from"])
	if rawFrom == "" {
		rawFrom = strings.TrimSpace(o["username"])
	}
	fromAddr, err := mail.ParseAddress(rawFrom)
	if err != nil {
		return fmt.Errorf("发件人地址无效: %w", err)
	}

	var toAddrs []*mail.Address
	var toHdr []string
	for _, x := range strings.Split(o["to"], ",") {
		x = strings.TrimSpace(x)
		if x == "" {
			continue
		}
		a, err := mail.ParseAddress(x)
		if err != nil {
			return fmt.Errorf("收件人地址无效 %q: %w", x, err)
		}
		toAddrs = append(toAddrs, a)
		toHdr = append(toHdr, a.String())
	}
	if len(toAddrs) == 0 {
		return fmt.Errorf("收件人为空")
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	var msg bytes.Buffer
	fmt.Fprintf(&msg, "From: %s\r\n", fromAddr.String())
	fmt.Fprintf(&msg, "To: %s\r\n", strings.Join(toHdr, ", "))
	fmt.Fprintf(&msg, "Subject: %s\r\n", encodeHeader(subject))
	fmt.Fprintf(&msg, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&msg, "MIME-Version: 1.0\r\n")
	fmt.Fprintf(&msg, "Content-Type: text/plain; charset=UTF-8\r\n")
	fmt.Fprintf(&msg, "Content-Transfer-Encoding: 8bit\r\n\r\n")
	msg.WriteString(body)

	var auth smtp.Auth
	if o["username"] != "" {
		auth = smtp.PlainAuth("", o["username"], o["password"], host)
	}

	const smtpTimeout = 60 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	rawConn, err := currentNotifyPolicy().DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("SMTP 连接失败: %w", err)
	}
	defer rawConn.Close()
	_ = rawConn.SetDeadline(time.Now().Add(smtpTimeout))

	if port == 465 {
		tlsConn := tls.Client(rawConn, &tls.Config{ServerName: host})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return fmt.Errorf("TLS 连接失败: %w", err)
		}
		cli, err := smtp.NewClient(tlsConn, host)
		if err != nil {
			return err
		}
		defer cli.Close()
		return smtpSend(cli, auth, fromAddr.Address, toAddrs, msg.Bytes())
	}

	cli, err := smtp.NewClient(rawConn, host)
	if err != nil {
		return err
	}
	defer cli.Close()
	ok, _ := cli.Extension("STARTTLS")
	if ok {
		if err := cli.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return fmt.Errorf("STARTTLS 失败: %w", err)
		}
		_ = rawConn.SetDeadline(time.Now().Add(smtpTimeout))
	} else {
		// 没有 STARTTLS 时，凭据和告警内容都会明文过网。
		// 本地中继可接受，其余情况一律拒绝，避免静默降级。
		if auth != nil {
			return fmt.Errorf("SMTP 服务器不支持 STARTTLS，拒绝明文发送凭据")
		}
		if !isLoopbackHost(host) {
			return fmt.Errorf("SMTP 服务器不支持 STARTTLS，拒绝明文发送邮件；请改用 465 端口或启用 STARTTLS")
		}
	}
	return smtpSend(cli, auth, fromAddr.Address, toAddrs, msg.Bytes())
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func smtpSend(cli *smtp.Client, auth smtp.Auth, from string, to []*mail.Address, msg []byte) error {
	if auth != nil {
		if err := cli.Auth(auth); err != nil {
			return fmt.Errorf("SMTP 认证失败: %w", err)
		}
	}
	if err := cli.Mail(from); err != nil {
		return err
	}
	for _, t := range to {
		if err := cli.Rcpt(t.Address); err != nil {
			return err
		}
	}
	w, err := cli.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return cli.Quit()
}

// encodeHeader 处理非 ASCII 主题（RFC 2047），并清除 CR/LF 防止头注入。
func encodeHeader(s string) string {
	s = strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
	ascii := true
	for _, r := range s {
		if r > 127 {
			ascii = false
			break
		}
	}
	if ascii {
		return s
	}
	return "=?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(s)) + "?="
}

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

	"github.com/motao123/rollcall/internal/netguard"
)

// sevRank 用于 MinSeverity 过滤。
var sevRank = map[string]int{"info": 1, "warn": 2, "critical": 3}

// SeverityRank 返回严重度排序值（未知为 0）。
func SeverityRank(s string) int { return sevRank[s] }

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

	// 通知降噪与扩展。旧配置没有这些键，Normalize 会补默认值。
	QuietStart  int    `json:"quietStart"`    // 免打扰起始小时 0–23；-1 关闭
	QuietEnd    int    `json:"quietEnd"`      // 免打扰结束小时 0–23
	DigestMins  int    `json:"digestMins"`    // 摘要聚合窗口（分钟），0=逐条发送
	TitleFormat string `json:"titleFormat"`   // 自定义标题模板，空=默认
	BodyFormat  string `json:"bodyFormat"`    // 自定义正文模板，空=默认
	ExpiryDays  int    `json:"expiryDays"`    // 到期提醒提前天数；<0 关闭
	ReportDaily bool   `json:"reportDaily"`   // 每日运行报告
	ReportWeek  bool   `json:"reportWeekly"`  // 每周运行报告
	ReportMonth bool   `json:"reportMonthly"` // 每月运行报告
	ReportHour  int    `json:"reportHour"`    // 报告发送小时（本地时区）
	// OfflineGraceSec 是离线类规则的全局宽限下限（秒）：只抬高不缩短，
	// 规则自身 Duration 更长时以规则为准；0=不干预（保持每规则现状）。
	OfflineGraceSec int `json:"offlineGraceSec"`
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

// defaults 返回新配置的默认值。旧配置缺少新增字段时，
// json.Unmarshal 会保留这里的值，避免被读成 0（例如免打扰 0–0 点）。
func defaults() *Config {
	return &Config{MinSeverity: "warn", QuietStart: -1, ExpiryDays: 7, ReportHour: 9}
}

// Parse 解析配置；自动把 v1 的三通道结构迁移为通用的 channels 列表。
func Parse(raw string) (*Config, error) {
	c := defaults()
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
			opts := map[string]string{
				"smtpHost": lg.Email.SMTPHost,
				"username": lg.Email.Username,
				"password": lg.Email.Password,
				"from":     lg.Email.From,
				"to":       strings.Join(lg.Email.To, ","),
			}
			// 旧配置省略端口时 SMTPPort 为 0。写成字符串 "0" 会让 sendEmail
			// 进入「非空即校验」分支并返回 SMTP 端口无效，从而绕过 465 默认值，
			// 迁移后的邮件渠道全部失效。只有显式配置了端口才写入。
			if lg.Email.SMTPPort > 0 {
				opts["smtpPort"] = strconv.Itoa(lg.Email.SMTPPort)
			}
			c.Channels = append(c.Channels, ChannelConfig{
				ID: "m_email", Type: "email", Name: "邮件",
				Enabled: lg.Email.Enabled,
				Options: opts,
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
	// 离线宽限下限：上限对齐最长规则时长（7 天），负数视为关闭。
	if c.OfflineGraceSec < 0 {
		c.OfflineGraceSec = 0
	}
	if c.OfflineGraceSec > 7*86400 {
		c.OfflineGraceSec = 7 * 86400
	}
	// 免打扰：-1 关闭；起止相同视为关闭（旧配置两个字段都是 0）。
	if c.QuietStart < 0 {
		c.QuietStart = -1
	} else if c.QuietStart > 23 {
		c.QuietStart = 23
	}
	if c.QuietEnd < 0 || c.QuietEnd > 23 {
		c.QuietEnd = 0
	}
	if c.QuietStart >= 0 && c.QuietStart == c.QuietEnd {
		c.QuietStart = -1
	}
	// 摘要窗口：0 表示逐条发送；非 0 时至少 5 分钟，否则退化成每条都触发一次汇总。
	if c.DigestMins != 0 {
		if c.DigestMins < 5 {
			c.DigestMins = 5
		}
		if c.DigestMins > 1440 {
			c.DigestMins = 1440
		}
	}
	c.TitleFormat = strings.TrimSpace(c.TitleFormat)
	c.BodyFormat = strings.TrimSpace(c.BodyFormat)
	if len([]rune(c.TitleFormat)) > 500 {
		c.TitleFormat = string([]rune(c.TitleFormat)[:500])
	}
	if len([]rune(c.BodyFormat)) > 2000 {
		c.BodyFormat = string([]rune(c.BodyFormat)[:2000])
	}
	if c.ExpiryDays < -1 {
		c.ExpiryDays = -1
	}
	if c.ExpiryDays > 365 {
		c.ExpiryDays = 365
	}
	if c.ReportHour < 0 || c.ReportHour > 23 {
		c.ReportHour = 9
	}
}

// QuietWindow 判断 now 是否处于免打扰时段，返回结束时间。
// 起止跨越午夜（如 23 → 7）时按两段处理。
func (c *Config) QuietWindow(now time.Time) (time.Time, bool) {
	if c == nil || c.QuietStart < 0 || c.QuietStart == c.QuietEnd {
		return time.Time{}, false
	}
	loc := now.Location()
	start := time.Date(now.Year(), now.Month(), now.Day(), c.QuietStart, 0, 0, 0, loc)
	end := time.Date(now.Year(), now.Month(), now.Day(), c.QuietEnd, 0, 0, 0, loc)
	if c.QuietStart < c.QuietEnd {
		if now.Before(start) || !now.Before(end) {
			return time.Time{}, false
		}
		return end, true
	}
	// 跨午夜：now 在 start 之后或 end 之前都算免打扰。
	if !now.Before(start) {
		return end.AddDate(0, 0, 1), true
	}
	if now.Before(end) {
		return end, true
	}
	return time.Time{}, false
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
		// 空 ID 会让 outbox 任务无法定位通道，也会让前端回传的掩码
		// 无法按通道还原；必须在前端生成稳定 ID 后再保存。
		if strings.TrimSpace(ch.ID) == "" {
			return fmt.Errorf("第 %d 个通道缺少 ID", i+1)
		}
		if seen[ch.ID] {
			return fmt.Errorf("通道 ID 重复：%s", ch.ID)
		}
		seen[ch.ID] = true
	}
	return nil
}

// Redacted 返回脱敏副本（敏感字段以掩码替换），用于下发前端。
func (c *Config) Redacted() *Config {
	cp := &Config{
		Enabled:         c.Enabled,
		MinSeverity:     c.MinSeverity,
		RepeatInterval:  c.RepeatInterval,
		QuietStart:      c.QuietStart,
		QuietEnd:        c.QuietEnd,
		DigestMins:      c.DigestMins,
		TitleFormat:     c.TitleFormat,
		BodyFormat:      c.BodyFormat,
		ExpiryDays:      c.ExpiryDays,
		ReportDaily:     c.ReportDaily,
		ReportWeek:      c.ReportWeek,
		ReportMonth:     c.ReportMonth,
		ReportHour:      c.ReportHour,
		OfflineGraceSec: c.OfflineGraceSec,
		Channels:        make([]ChannelConfig, 0, len(c.Channels)),
	}
	for _, ch := range c.Channels {
		sk := secretKeys(ch.Type)
		// 未知类型时 secretKeys 返回 nil，无法判断哪些字段敏感；
		// 宁可将全部非空字段脱敏，也不能把潜在凭据明文下发。
		_, known := defOf(ch.Type)
		opts := make(map[string]string, len(ch.Options))
		for k, v := range ch.Options {
			if v != "" && (!known || sk[k]) {
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

// mask 生成下发到前端的脱敏占位符。
//
// 阈值不能太低：11–20 字符的凭据（例如 QQ 邮箱 16 位 SMTP 授权码、
// 各类 webhook token）若保留首尾各 4 位，等于把一半凭据发给浏览器；
// 前端页面一旦被截图或经浏览器插件读取就构成泄露。
func mask(s string) string {
	if len(s) < 24 {
		return "********"
	}
	return s[:4] + "********" + s[len(s)-4:]
}

func isMasked(s string) bool { return strings.Contains(s, "********") }

// MergeSecrets 把前端回传的掩码占位符还原为已保存的真实值。
// 按「通道 ID + 类型」匹配，类型不一致则不复用，避免串到别的通道上。
//
// 反方向同样重要：**找不到可复用旧值时，占位符必须被清空**。
// 否则 "abcd********wxyz" 会被当成真实密钥落库，之后每次投递都用它去请求，
// 报错是「认证失败」而不是「密钥未填」，排查方向完全被带偏。
func (c *Config) MergeSecrets(old *Config) {
	prev := map[string]ChannelConfig{}
	if old != nil {
		for _, ch := range old.Channels {
			if ch.ID == "" {
				continue
			}
			prev[ch.ID] = ch
		}
	}
	for i := range c.Channels {
		ch := &c.Channels[i]
		// 无 ID 的通道无法与旧配置一一对应；按空串合并会让多个空 ID 通道
		// 互相覆盖掩码，导致密钥被清空或串用。
		o, ok := prev[ch.ID]
		if ch.ID == "" || !ok || o.Type != ch.Type {
			dropMaskedSecrets(ch)
			continue
		}
		sk := secretKeys(ch.Type)
		for k, v := range ch.Options {
			if !sk[k] || !isMasked(v) {
				continue
			}
			if real, ok := o.Options[k]; ok && real != "" && !isMasked(real) {
				ch.Options[k] = real
			} else {
				dropOption(ch, k)
			}
		}
	}
}

// dropMaskedSecrets 清掉通道里所有仍是占位符的敏感项。
func dropMaskedSecrets(ch *ChannelConfig) {
	for k, v := range ch.Options {
		if isMasked(v) {
			dropOption(ch, k)
		}
	}
}

func dropOption(ch *ChannelConfig, k string) {
	if ch.Options == nil {
		return
	}
	delete(ch.Options, k)
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

	subject, body := renderWith(cfg, m)
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

// renderWith 在默认渲染基础上应用自定义模板。
// 模板使用 {status}/{node}/{title}/{rule}/{value}/{detail}/{severity}/{time}/{link} 占位符。
func renderWith(cfg *Config, m Message) (subject, body string) {
	subject, body = render(m)
	if cfg == nil {
		return subject, body
	}
	if cfg.TitleFormat != "" {
		subject = expandTemplate(cfg.TitleFormat, m)
	}
	if cfg.BodyFormat != "" {
		body = expandTemplate(cfg.BodyFormat, m)
	}
	return subject, body
}

func expandTemplate(tpl string, m Message) string {
	status := "告警"
	if m.Resolved {
		status = "恢复"
	}
	value := ""
	if m.Value != 0 {
		value = fmt.Sprintf("%.1f", m.Value)
	}
	return strings.NewReplacer(
		"{status}", status,
		"{node}", m.NodeName,
		"{title}", m.Title,
		"{rule}", m.RuleName,
		"{value}", value,
		"{detail}", m.Body,
		"{severity}", m.Severity,
		"{time}", time.Unix(m.TS, 0).Format("2006-01-02 15:04:05"),
		"{link}", m.Link,
	).Replace(tpl)
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
		return sendWebhook(ch.Options, subject, body, m)
	}
	if p, ok := pluginOf(ch.Type); ok {
		return runPlugin(p, ch, subject, body, m)
	}
	return fmt.Errorf("不支持的通道类型：%s", ch.Type)
}

func postJSON(urlStr string, payload any) ([]byte, error) {
	// 校验与真正请求必须使用同一个字符串：前端粘贴地址常带首尾空格，
	// 只对校验侧 TrimSpace 会出现「校验通过但请求失败」的怪现象。
	urlStr = strings.TrimSpace(urlStr)
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
		// 不包装原始错误：*url.Error 的文本会带上完整 URL，
		// 而 Telegram 的 bot token 位于路径、通用 Webhook 的密钥常写在查询串，
		// 回显到 API 响应或日志即泄露凭据（与 netErr 的取舍一致）。
		return errors.New("通知地址格式无效")
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
		ErrCode *int   `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := decodeResult(rb, &r); err != nil {
		return fmt.Errorf("wecom: %w", err)
	}
	if r.ErrCode == nil {
		// 缺字段时零值 0 会被当成成功；必须要求服务端显式返回结果。
		return fmt.Errorf("wecom: 响应缺少 errcode，无法确认投递结果")
	}
	if *r.ErrCode != 0 {
		return fmt.Errorf("wecom: %s (errcode %d)", r.ErrMsg, *r.ErrCode)
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
		ErrCode *int   `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := decodeResult(rb, &r); err != nil {
		return fmt.Errorf("dingtalk: %w", err)
	}
	if r.ErrCode == nil {
		return fmt.Errorf("dingtalk: 响应缺少 errcode，无法确认投递结果")
	}
	if *r.ErrCode != 0 {
		return fmt.Errorf("dingtalk: %s (errcode %d)", r.ErrMsg, *r.ErrCode)
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
		Code *int   `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := decodeResult(rb, &r); err != nil {
		return fmt.Errorf("lark: %w", err)
	}
	if r.Code == nil {
		return fmt.Errorf("lark: 响应缺少 code，无法确认投递结果")
	}
	if *r.Code != 0 {
		return fmt.Errorf("lark: %s (code %d)", r.Msg, *r.Code)
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
	u, err := url.Parse(server)
	if err != nil || u.Host == "" {
		return fmt.Errorf("bark: serverUrl 无效")
	}
	// Bark 的 JSON 推送接口是 POST <server>/push；根路径只提供 GET 网页，
	// 直接 POST 根路径会得到 404/405。反代前缀（如 /bark）后同样要拼 /push；
	// 已显式以 /push 结尾的地址保持不变。
	if p := strings.TrimRight(u.Path, "/"); !strings.HasSuffix(p, "/push") {
		u.Path = p + "/push"
	}
	payload := map[string]any{"title": subject, "body": body}
	if k := o["deviceKey"]; k != "" {
		payload["device_key"] = k
	}
	if g := o["group"]; g != "" {
		payload["group"] = g
	}
	rb, err := postJSON(u.String(), payload)
	if err != nil {
		return err
	}
	var r struct {
		Code    *int   `json:"code"`
		Message string `json:"message"`
	}
	if err := decodeResult(rb, &r); err != nil {
		return fmt.Errorf("bark: %w", err)
	}
	if r.Code == nil {
		return fmt.Errorf("bark: 响应缺少 code，无法确认投递结果")
	}
	if *r.Code != 0 && *r.Code != 200 {
		return fmt.Errorf("bark: %s (code %d)", r.Message, *r.Code)
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
		Code *int   `json:"code"`
		Msg  string `json:"message"`
	}
	if err := decodeResult(rb, &r); err != nil {
		return fmt.Errorf("serverchan: %w", err)
	}
	if r.Code == nil {
		return fmt.Errorf("serverchan: 响应缺少 code，无法确认投递结果")
	}
	if *r.Code != 0 {
		return fmt.Errorf("serverchan: %s (code %d)", r.Msg, *r.Code)
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
		Code *int   `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := decodeResult(rb, &r); err != nil {
		return fmt.Errorf("pushplus: %w", err)
	}
	if r.Code == nil {
		return fmt.Errorf("pushplus: 响应缺少 code，无法确认投递结果")
	}
	if *r.Code != 200 {
		return fmt.Errorf("pushplus: %s (code %d)", r.Msg, *r.Code)
	}
	return nil
}

// ---------- Gotify ----------

func sendGotify(o map[string]string, subject, body string) error {
	server := strings.TrimRight(strings.TrimSpace(o["serverUrl"]), "/")
	prio := 5
	if v := strings.TrimSpace(o["priority"]); v != "" {
		// 静默回退会掩盖拼写错误：用户以为配了 10，实际一直发 5。
		n, err := strconv.Atoi(v)
		if err != nil || n < -2 || n > 10 {
			return fmt.Errorf("gotify: priority 必须是 -2 到 10 的整数")
		}
		prio = n
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
	// Gotify 的推送鉴权是 X-Gotify-Key 头（或 token 查询参数），
	// 不接受 Authorization: Bearer —— 用错头会稳定返回 401。
	if tok := strings.TrimSpace(o["token"]); tok != "" {
		req.Header.Set("X-Gotify-Key", tok)
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

// ---------- 通用 Webhook ----------

func sendWebhook(o map[string]string, subject, body string, m Message) error {
	// Webhook 也必须遵守自定义模板：Subject/Body 是 renderWith 的渲染结果，
	// 直接原样序列化 m 会让模板对 Webhook 通道静默失效。
	msg := m
	msg.Title = subject
	msg.Body = body
	b, err := json.Marshal(msg)
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
	// host:port 必须拒绝：net.JoinHostPort 会把 "host:587" 当成 IPv6
	// 再套一层括号，得到 "[host:587]:465"，连接必然失败。
	// IPv6 字面量（可带方括号）仍允许，JoinHostPort 会正确补括号。
	host := strings.TrimSpace(o["smtpHost"])
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	if host == "" || strings.ContainsAny(host, " \r\n\t/") {
		return fmt.Errorf("SMTP 主机无效（只填主机名，端口用 smtpPort 单独配置）")
	}
	if strings.Contains(host, ":") && net.ParseIP(host) == nil {
		return fmt.Errorf("SMTP 主机无效（只填主机名，端口用 smtpPort 单独配置）")
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

	// ★ 用 ParseAddressList 而不是按逗号 Split：
	// `"Doe, John" <john@example.com>` 这种带引号显示名的地址里就有逗号，
	// 切开后两半都不是合法地址，配置直接被判为无效。
	list, err := mail.ParseAddressList(o["to"])
	if err != nil {
		return fmt.Errorf("收件人地址无效: %w", err)
	}
	var toAddrs []*mail.Address
	var toHdr []string
	for _, a := range list {
		if a == nil || strings.TrimSpace(a.Address) == "" {
			continue
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
	// 走到这里说明 DATA 事务已收到服务端的 250，邮件已经被接收。
	// 之后的 QUIT 只是礼貌性关闭：部分服务端在收下邮件后直接断开，
	// 网络抖动也常见。把 QUIT 失败当成投递失败会造成重复投递 ——
	// 收件人收到两条同样的告警，而告警系统还报告失败。
	_ = cli.Quit()
	return nil
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

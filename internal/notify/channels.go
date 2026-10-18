// Package notify 实现告警通知投递。
//
// 设计参考：
//   - usual2970/certimate  通道抽象 + 每通道独立配置（Go）
//   - netcccyun/dnsmgr     覆盖国内主流推送渠道（企业微信/钉钉/飞书/Server酱…）
//   - mouday/domain-admin  配置与测试分离，支持逐通道自检
//
// 全部使用标准库实现，不引入第三方依赖。
//
// 通道清单：企业微信 / 钉钉 / 飞书 / Telegram / Bark / Server酱 /
//
//	PushPlus / Gotify / 邮件(SMTP) / 通用 Webhook
package notify

// Field 描述一个配置字段，前端据此动态渲染表单。
type Field struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Type        string `json:"type"` // text | password | number | url
	Placeholder string `json:"placeholder"`
	Required    bool   `json:"required"`
	Secret      bool   `json:"secret"` // 为 true 时下发前端会脱敏
	Hint        string `json:"hint"`
	Default     string `json:"default"`
}

// ChannelDef 是一个通道类型的能力声明。
type ChannelDef struct {
	Type   string  `json:"type"`
	Label  string  `json:"label"`
	Desc   string  `json:"desc"`
	Fields []Field `json:"fields"`
}

// channelDefs 是静态通道定义。构建一次即可：Defs/defOf/secretKeys
// 都会在每次投递时被调用，反复重建整张表纯属浪费。
var channelDefs = []ChannelDef{
	{
		Type: "wecom", Label: "企业微信群机器人",
		Desc: "群设置 → 群机器人 → 添加机器人，复制 Webhook 地址",
		Fields: []Field{
			{Key: "webhookUrl", Label: "Webhook 地址", Type: "password", Required: true, Secret: true,
				Placeholder: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=…"},
		},
	},
	{
		Type: "dingtalk", Label: "钉钉群机器人",
		Desc: "群设置 → 智能群助手 → 添加机器人 → 自定义，安全设置选「加签」",
		Fields: []Field{
			{Key: "webhookUrl", Label: "Webhook 地址", Type: "password", Required: true, Secret: true,
				Placeholder: "https://oapi.dingtalk.com/robot/send?access_token=…"},
			{Key: "secret", Label: "加签密钥（可选）", Type: "password", Secret: true,
				Hint: "安全设置选「加签」时必填，形如 SECxxxx"},
		},
	},
	{
		Type: "lark", Label: "飞书群机器人",
		Desc: "群设置 → 群机器人 → 添加机器人 → 自定义机器人",
		Fields: []Field{
			{Key: "webhookUrl", Label: "Webhook 地址", Type: "password", Required: true, Secret: true,
				Placeholder: "https://open.feishu.cn/open-apis/bot/v2/hook/…"},
			{Key: "secret", Label: "签名密钥（可选）", Type: "password", Secret: true,
				Hint: "开启「签名校验」时填写"},
		},
	},
	{
		Type: "telegram", Label: "Telegram",
		Desc: "找 @BotFather 创建机器人拿 Token，向机器人发消息后从 getUpdates 取 Chat ID",
		Fields: []Field{
			{Key: "botToken", Label: "Bot Token", Type: "password", Required: true, Secret: true,
				Placeholder: "123456789:AAH…"},
			{Key: "chatId", Label: "Chat ID", Type: "text", Required: true,
				Placeholder: "-1001234567890"},
		},
	},
	{
		Type: "bark", Label: "Bark（iOS）",
		Desc: "Bark App 首页可看到设备 Key；可自建服务端",
		Fields: []Field{
			{Key: "deviceKey", Label: "设备 Key", Type: "password", Required: true, Secret: true,
				Placeholder: "从 Bark App 复制"},
			{Key: "serverUrl", Label: "服务端地址（可选）", Type: "url",
				Placeholder: "https://api.day.app/", Default: "https://api.day.app/",
				Hint: "留空使用官方服务"},
			{Key: "group", Label: "分组（可选）", Type: "text", Placeholder: "Rollcall"},
		},
	},
	{
		Type: "serverchan", Label: "Server酱（方糖）",
		Desc: "sct.ftqq.com 登录后获取 SendKey",
		Fields: []Field{
			{Key: "sendKey", Label: "SendKey", Type: "password", Required: true, Secret: true,
				Placeholder: "SCT…"},
		},
	},
	{
		Type: "pushplus", Label: "PushPlus",
		Desc: "pushplus.plus 微信扫码登录后获取 token",
		Fields: []Field{
			{Key: "token", Label: "Token", Type: "password", Required: true, Secret: true},
			{Key: "topic", Label: "群组编码（可选）", Type: "text",
				Hint: "一对多推送时填写"},
		},
	},
	{
		Type: "gotify", Label: "Gotify（自建）",
		Desc: "自建 Gotify 服务，在「应用」页创建 Token",
		Fields: []Field{
			{Key: "serverUrl", Label: "服务地址", Type: "url", Required: true,
				Placeholder: "https://gotify.example.com"},
			{Key: "token", Label: "应用 Token", Type: "password", Required: true, Secret: true},
			{Key: "priority", Label: "优先级", Type: "number", Default: "5",
				Hint: "1-10，越大越靠前"},
		},
	},
	{
		Type: "email", Label: "邮件（SMTP）",
		Desc: "支持 465 隐式 TLS 与 587 STARTTLS",
		Fields: []Field{
			{Key: "smtpHost", Label: "SMTP 主机", Type: "text", Required: true, Placeholder: "smtp.qq.com"},
			{Key: "smtpPort", Label: "端口", Type: "number", Default: "465", Hint: "465=SSL，587=STARTTLS"},
			{Key: "username", Label: "用户名", Type: "text", Placeholder: "you@qq.com"},
			{Key: "password", Label: "密码/授权码", Type: "password", Secret: true},
			{Key: "from", Label: "发件人", Type: "text", Hint: "留空则用用户名"},
			{Key: "to", Label: "收件人", Type: "text", Required: true,
				Placeholder: "a@x.com,b@y.com", Hint: "多个用英文逗号分隔"},
		},
	},
	{
		Type: "webhook", Label: "通用 Webhook",
		Desc: "以 JSON POST 投递，可对接自建服务或自动化平台",
		Fields: []Field{
			{Key: "url", Label: "URL", Type: "url", Required: true,
				Placeholder: "https://your-endpoint/hook"},
			{Key: "secret", Label: "密钥（可选）", Type: "password", Secret: true,
				Hint: "放在 X-Rollcall-Secret 请求头"},
		},
	},
}

// Defs 返回全部可用通道定义（深拷贝 Fields，防止调用方篡改内置表）。
func Defs() []ChannelDef {
	out := make([]ChannelDef, len(channelDefs))
	for i, d := range channelDefs {
		d.Fields = append([]Field(nil), d.Fields...)
		out[i] = d
	}
	return out
}

// defOf 按类型查找定义。
func defOf(t string) (ChannelDef, bool) {
	for _, d := range channelDefs {
		if d.Type == t {
			return d, true
		}
	}
	return ChannelDef{}, false
}

// secretKeys 返回某通道的敏感字段名集合。
func secretKeys(t string) map[string]bool {
	d, ok := defOf(t)
	if !ok {
		return nil
	}
	m := map[string]bool{}
	for _, f := range d.Fields {
		if f.Secret {
			m[f.Key] = true
		}
	}
	return m
}

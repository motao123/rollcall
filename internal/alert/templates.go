package alert

import "github.com/motao123/rollcall/internal/model"

// RuleTemplate 是一条可一键套用的规则预设。
//
// 与 DefaultRules 同源：模板直接复用默认规则的 ID 与阈值，
// 这样「补齐缺失模板」天然幂等 —— 已存在的 ID 由调用方标记为已应用，
// 不会因为点两次而生成两条同名规则；也避免默认规则改了阈值、
// 模板里还留着旧值的漂移。
type RuleTemplate struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Desc      string  `json:"desc"`
	Metric    string  `json:"metric"`
	Op        string  `json:"op"`
	Threshold float64 `json:"threshold"`
	Duration  int     `json:"duration"`
	Severity  string  `json:"severity"`
	// ServiceOnly 表示该模板只对服务探测有意义：
	// 表单里应隐藏「定向节点 / 分组」，改用「定向服务」。
	ServiceOnly bool `json:"serviceOnly,omitempty"`
	// Applied 由调用方填充：同 ID 的规则已经存在。
	Applied bool `json:"applied,omitempty"`
}

// templateDesc 是各模板的说明文案。
//
// 说明里必须写清「为什么是这个阈值 / 这个时长」，否则用户不知道
// 90% 和 300 秒从哪来，后续调阈值也没有参照。
// 键与 DefaultRules 的 Rule.ID 对应，缺失时说明留空（仍可用），
// 由 templates_test.go 保证不遗漏。
var templateDesc = map[string]struct {
	Desc        string
	ServiceOnly bool
}{
	"r_offline": {
		Desc: "Agent 每 30 秒上报一次，连续 3 分钟没有任何上报才判失联：" +
			"太短会被网络抖动打断（一次 20 秒的丢包就误报），太长则失去告警意义。",
	},
	"r_cpu": {
		Desc: "CPU 持续 5 分钟高于 90%。用「持续」而不是瞬时，是为了躲开编译、备份、" +
			"日志轮转这类短时冲高；单核机型可降到 85%，多核机型可放宽到 95%。",
	},
	"r_mem": {
		Desc: "内存 5 分钟高于 90%。留 10% 余量是给内核页缓存与突发进程；" +
			"有 Swap 的机器先看 Swap 是否同步上涨 —— 那说明已经在换页，不是缓存。",
	},
	"r_disk": {
		Desc: "磁盘 10 分钟高于 85%，级别定为严重：写满后数据库、日志、包管理器都会失败，" +
			"而且清理需要登录操作。留 15% 是给临时文件与快照的缓冲。",
	},
	"r_service_down": {
		Desc: "HTTP/TCP/DNS 探测失败持续 2 分钟即报。单次失败可能只是探测侧网络抖动，" +
			"连续两分钟失败基本可以确认服务侧有问题。",
		ServiceOnly: true,
	},
	"r_cert_days": {
		Desc: "证书剩余不足 14 天告警。Let's Encrypt 证书 90 天有效，" +
			"14 天足够发现「自动续期静默失效」并人工介入。",
		ServiceOnly: true,
	},
	"r_traffic_quota": {
		Desc: "月度配额用掉 100% 时告警。想提前预警可把阈值改成 90，" +
			"用量口径（收发合计 / 只计下行 / 取较大者）在节点台账里按商家计费方式设置。",
	},
}

// RuleTemplates 返回内置规则模板。
func RuleTemplates() []RuleTemplate {
	defaults := DefaultRules()
	out := make([]RuleTemplate, 0, len(defaults))
	for _, r := range defaults {
		meta := templateDesc[r.ID]
		out = append(out, RuleTemplate{
			ID:          r.ID,
			Name:        r.Name,
			Desc:        meta.Desc,
			Metric:      r.Metric,
			Op:          r.Op,
			Threshold:   r.Threshold,
			Duration:    r.Duration,
			Severity:    r.Severity,
			ServiceOnly: meta.ServiceOnly,
		})
	}
	return out
}

// Rule 把模板转成待创建的规则实例。
// 模板本身不带定向范围：套用后作用于全部节点，用户再按需收窄。
func (t RuleTemplate) Rule() *model.AlertRule {
	return &model.AlertRule{
		ID:        t.ID,
		Name:      t.Name,
		Metric:    t.Metric,
		Op:        t.Op,
		Threshold: t.Threshold,
		Duration:  t.Duration,
		Severity:  t.Severity,
		Enabled:   true,
	}
}

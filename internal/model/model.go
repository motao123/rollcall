// Package model 定义所有跨组件共享的数据结构。
// 设计约束：只包含数据结构与纯函数，不引入任何外部依赖。
package model

import (
	"encoding/json"
	"strings"
)

// Node 是一个被监控节点的台账条目。
// 它同时承担两个职责：监控节点的元信息、以及资产台账（服务商/到期/价格）。
type Node struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Provider string  `json:"provider"` // 服务商：racknerd / netcup / 棉花云…
	Region   string  `json:"region"`   // 地区
	Group    string  `json:"group"`    // 分组：prod / test / probe
	Note     string  `json:"note"`
	ExpireAt string  `json:"expireAt"` // YYYY-MM-DD，到期日
	Price    float64 `json:"price"`    // 年付续费价格（月成本按 /12 估算）
	Currency string  `json:"currency"` // USD / CNY
	Weight   int     `json:"weight"`   // 排序权重，越小越靠前
	// Country 是 ISO 3166-1 alpha-2 国家代码，用于公开页分布图。
	// 留空表示不在地图上展示。
	Country  string `json:"country,omitempty"`
	Favorite bool   `json:"favorite,omitempty"`
	// AlertExempt 是台账级告警豁免：经常计划重启/迁移中的机器开了它，
	// 任何节点指标规则都不再评估该节点，既有活跃告警以「节点告警豁免」为由关闭。
	AlertExempt bool `json:"alertExempt,omitempty"`
	// Tags 是自由标签，用于筛选与公开状态页分组展示。
	Tags []string `json:"tags,omitempty"`
	// TrafficQuotaGB 是每个流量周期的配额（GB，0 表示不限制）。
	// TrafficResetDay 是周期起始日（1–28，0 按 1 处理）。
	TrafficQuotaGB  float64 `json:"trafficQuotaGb,omitempty"`
	TrafficResetDay int     `json:"trafficResetDay,omitempty"`
	// TrafficMode 决定周期用量如何由收发字节合成：sum（默认，收发合计）/
	// rx（只计下行）/ tx（只计上行）/ max（取较大者）。
	// 商家按单边计费时选对应口径，否则双边相加会高估用量、提前触发配额告警。
	TrafficMode string `json:"trafficMode,omitempty"`
	// AgentVersion 是 Agent 最近一次上报的版本，由上报请求头写入，
	// 台账 API 不能修改；用于面板提示落后的 Agent。
	AgentVersion string `json:"agentVersion,omitempty"`
	// LastIP 是最近一次通过鉴权上报的来源 IP，由服务端写入，台账 API 不能修改。
	// 只出现在管理端接口：公开页用独立的脱敏视图，导出也按列白名单生成。
	// 用途是排查「这台机器到底从哪个地址连过来的」，也是批量运维时定位主机的线索。
	LastIP string `json:"lastIP,omitempty"`
	// Signed 表示最近一次被接受的上报带有效签名（v1.16.0 起的 Agent 才发）。
	// 由服务端写入，台账 API 不能修改；用于在"要求签名"前后看谁还没升上来。
	Signed bool `json:"signed,omitempty"`
	// ClockSkew 是 Agent 自报时间与服务端时间的差值（秒，Agent − 服务端）。
	// 只在收到签名上报时更新：未签名的上报里那个 ts 本身就不受信任。
	ClockSkew int64 `json:"clockSkew,omitempty"`
	// NICSet 是最近一次上报所统计网卡集合的指纹（Agent 计算，经请求头带来）。
	// 集合变化意味着累计计数器的口径变了（WARP/多 WAN 切换、网卡热插拔），
	// 服务端据此重建流量基线并标记周期不完整，防止跳变被记成真实流量。
	NICSet string `json:"nicSet,omitempty"`
	// LastLogin 是最近一次被记录的交互式登录（由上报写入，台账 API 不可改）。
	// 同时用于通知去重：同一节点同一 (ts,user,host) 只推一次。
	LastLogin *LoginEvent `json:"lastLogin,omitempty"`
	// LoginState/LoginAge 是 Agent 自报的登录采集可用性（请求头写入）。
	// 取值 wtmp / wtmp-stale / unavailable；Age 是 wtmp 最后写入距今的秒数，
	// 读不到时为 -1。面板据此显示"最近写入 N 天前"，而不是静默不触发。
	LoginState  string `json:"loginState,omitempty"`
	LoginAge    int64  `json:"loginAge,omitempty"`
	LoginLastTS int64  `json:"loginLastTs,omitempty"`
	Token       string `json:"-"` // 上报凭证，永不下发给前端
}

// MountInfo 是一个文件系统的用量快照。
type MountInfo struct {
	Path  string `json:"path"`
	Used  uint64 `json:"used"`
	Total uint64 `json:"total"`
}

// HostInfo 是节点硬件与系统静态信息，由 Agent 首次上报后缓存。
type HostInfo struct {
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Kernel   string `json:"kernel"`
	Arch     string `json:"arch"`
	CPUModel string `json:"cpuModel"`
	Cores    int    `json:"cores"`
	Virt     string `json:"virt"`
}

// Metrics 是一次采集的指标快照。
type Metrics struct {
	TS        int64   `json:"ts"` // unix 秒
	CPUPct    float64 `json:"cpuPct"`
	Load1     float64 `json:"load1"`
	Load5     float64 `json:"load5"`
	Load15    float64 `json:"load15"`
	MemUsed   uint64  `json:"memUsed"`
	MemTotal  uint64  `json:"memTotal"`
	SwapUsed  uint64  `json:"swapUsed"`
	SwapTotal uint64  `json:"swapTotal"`
	DiskUsed  uint64  `json:"diskUsed"`
	DiskTotal uint64  `json:"diskTotal"`
	NetRx     uint64  `json:"netRx"` // 累计接收字节
	NetTx     uint64  `json:"netTx"` // 累计发送字节
	TCPConn   int     `json:"tcpConn"`
	Uptime    uint64  `json:"uptime"` // 秒
	Procs     int     `json:"procs"`
	// DiskReadBytes / DiskWriteBytes 是累计磁盘 I/O 字节数。
	DiskReadBytes  uint64 `json:"diskReadBytes,omitempty"`
	DiskWriteBytes uint64 `json:"diskWriteBytes,omitempty"`
	// StealPct / IowaitPct 是区间占比（0–100）。steal 高通常意味着宿主机
	// 超售把 CPU 时间片偷走了；两者只在原始区间有值（聚合桶未落盘）。
	StealPct  float64     `json:"stealPct,omitempty"`
	IowaitPct float64     `json:"iowaitPct,omitempty"`
	Mounts    []MountInfo `json:"mounts,omitempty"`
	NICs      []NICInfo   `json:"nics,omitempty"`
}

// NICInfo 是单块网卡的累计流量。
type NICInfo struct {
	Name string `json:"name"`
	Rx   uint64 `json:"rx"`
	Tx   uint64 `json:"tx"`
}

// TrafficCycle 是一个流量周期的累计用量。
// Agent 上报的 NetRx/NetTx 是自开机以来的累计值，重启会归零；
// 这里保存上一次观察值与周期内累计增量，计数器归零时从零重新累加。
type TrafficCycle struct {
	Start  int64  `json:"start"` // 当前周期起点（Unix 秒）
	LastRx uint64 `json:"lastRx"`
	LastTx uint64 `json:"lastTx"`
	UsedRx uint64 `json:"usedRx"`
	UsedTx uint64 `json:"usedTx"`
	// Incomplete 表示本周期经历过"计数口径变化"（网卡集合变更 / 计数器
	// 异常归零），用量数字可能与服务商面板对不上；「校正本期」可修正并清除。
	Incomplete bool `json:"incomplete,omitempty"`
	// NeedBaseline 表示「本周期已有人工补录，但还没有拿到真实计数器基线」。
	//
	// 手动校正可能发生在节点尚未上报过任何指标时（迁移过来还没装 Agent），
	// 此时 LastRx/LastTx 只能是 0。若不加这个标记，下一次上报会把
	// /proc/net/dev 的自开机累计值当成"本次增量"整段记进本期用量。
	// 旧快照没有这个字段（零值 false）= 基线已就绪，行为不变。
	NeedBaseline bool `json:"needBaseline,omitempty"`
}

// TrafficUsage 是节点当前流量周期的展示数据。
type TrafficUsage struct {
	UsedRx     uint64  `json:"usedRx"`
	UsedTx     uint64  `json:"usedTx"`
	QuotaGB    float64 `json:"quotaGb,omitempty"`
	ResetDay   int     `json:"resetDay,omitempty"`
	CycleStart int64   `json:"cycleStart,omitempty"`
	// Mode 是生效的流量口径（已归一化，空值不会出现）；
	// Used 是按该口径合成的计费用量，前端与告警都应使用它而不是自己相加。
	Mode string `json:"mode"`
	// Incomplete 表示本周期经历过计数口径变化（网卡集合变更等），
	// 用量可能不准；前端据此提示可用「校正本期」修正。
	Used       uint64 `json:"used"`
	Incomplete bool   `json:"incomplete,omitempty"`
}

// 流量口径取值。不同商家计费方式不同：有的收发相加，有的只算单边，
// 还有的取收发较大者，用错口径会让配额判断整体偏高或偏低。
const (
	TrafficModeSum = "sum" // 收发合计（默认，与历史行为一致）
	TrafficModeRx  = "rx"  // 只计下行（接收）
	TrafficModeTx  = "tx"  // 只计上行（发送）
	TrafficModeMax = "max" // 取收发较大者
)

// ValidTrafficMode 判断口径是否可接受；空字符串表示「未设置」，按默认处理。
func ValidTrafficMode(mode string) bool {
	switch mode {
	case "", TrafficModeSum, TrafficModeRx, TrafficModeTx, TrafficModeMax:
		return true
	}
	return false
}

// NormalizeTrafficMode 把「未设置」收敛成默认口径。
// 未知值不在这里猜测：校验层已经拦掉，此处只为空值兜底。
func NormalizeTrafficMode(mode string) string {
	if mode == "" {
		return TrafficModeSum
	}
	return mode
}

// TrafficUsed 按口径把周期内的收发字节合成计费用量。
// 计数器来自 /proc/net/dev 的累计值，现实中不可能接近 uint64 上限，
// 因此合计口径直接相加。
func TrafficUsed(mode string, rx, tx uint64) uint64 {
	switch NormalizeTrafficMode(mode) {
	case TrafficModeRx:
		return rx
	case TrafficModeTx:
		return tx
	case TrafficModeMax:
		if rx > tx {
			return rx
		}
		return tx
	default:
		return rx + tx
	}
}

// Report 是 Agent → Server 的单向上报载荷。
// 注意：协议中不存在任何服务端 → Agent 的指令通道。
type Report struct {
	NodeID  string   `json:"nodeId"`
	Host    HostInfo `json:"host"`
	Metrics Metrics  `json:"metrics"`
	// Logins 是"自上次上报以来的新登录"，正常情况下为空。
	//
	// ★ 这个字段决定了升级顺序：服务端用 DisallowUnknownFields 解码，
	// 所以**必须先升服务端、再升 Agent**（与上报签名那轮相反）。
	// 旧服务端收到带 logins 的上报会整份 400，节点直接失联。
	Logins []LoginEvent `json:"logins,omitempty"`
}

// LoginEvent 是一次成功登录（wtmp 的 USER_PROCESS 记录）。
// 失败尝试不写进 wtmp，因此这里代表"确实有人进来了"。
type LoginEvent struct {
	TS   int64  `json:"ts"`
	User string `json:"user"`
	Host string `json:"host"` // 来源地址；本地控制台登录为空
	Line string `json:"line"` // tty / pts/N
}

// Service 是一个服务探测目标（HTTP / TCP / DNS / ICMP）。
type Service struct {
	ID       string `json:"id"`
	NodeID   string `json:"nodeId"` // 归组到某个节点，用于展示
	Name     string `json:"name"`
	Type     string `json:"type"` // http | https | tcp | dns | icmp
	Target   string `json:"target"`
	Interval int    `json:"interval"` // 秒
	Enabled  bool   `json:"enabled"`
}

// ServiceResult 是一次探测结果。
type ServiceResult struct {
	ServiceID string `json:"serviceId"`
	TS        int64  `json:"ts"`
	OK        bool   `json:"ok"`
	LatencyMs int    `json:"latencyMs"`
	CertDays  int    `json:"certDays"` // TLS 证书剩余天数：-1=非 TLS，0=已过期，>0=剩余天数（不足一天按 1 计）
	Status    int    `json:"status"`   // HTTP 状态码，TCP 为 0
	Detail    string `json:"detail"`
}

// ServiceSample 是探测历史的存储形态。
//
// 早期实现把探测结果塞进 Metrics 的字段里：CPUPct 存延迟、TCPConn 存成功、
// Procs 存证书天数。那种"压缩存储"每读一处都要靠注释解释，而且任何人按字段名
// 理解都会静默得到错误的延迟数据 —— 现在拆成独立类型。
// 旧快照的兼容解析放在 UnmarshalJSON 里，升级不用丢探测历史。
type ServiceSample struct {
	TS        int64 `json:"ts"`
	LatencyMs int   `json:"lat"`
	OK        bool  `json:"ok"`
	CertDays  int   `json:"cert"` // -1=非 TLS，0=已过期，>0=剩余天数
}

// UnmarshalJSON 同时接受新格式与旧格式（Metrics 字段复用）。
//
// 用"某个键是否存在"来判形态，而不是看数值：旧的 cpuPct 完全可能是 0，
// 靠值判断会把 0 延迟的旧样本误判成新格式的空样本。
func (s *ServiceSample) UnmarshalJSON(b []byte) error {
	var raw struct {
		TS   int64 `json:"ts"`
		Lat  *int  `json:"lat"`
		OK   *bool `json:"ok"`
		Cert *int  `json:"cert"`
		// 旧格式（v1.17.2 及以前）
		CPU   *float64 `json:"cpuPct"`
		TCP   *int     `json:"tcpConn"`
		Procs *int     `json:"procs"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	s.TS = raw.TS
	switch {
	case raw.Lat != nil:
		s.LatencyMs = *raw.Lat
	case raw.CPU != nil:
		s.LatencyMs = int(*raw.CPU)
	}
	switch {
	case raw.OK != nil:
		s.OK = *raw.OK
	case raw.TCP != nil:
		s.OK = *raw.TCP == 1
	}
	switch {
	case raw.Cert != nil:
		s.CertDays = *raw.Cert
	case raw.Procs != nil:
		s.CertDays = *raw.Procs
	default:
		s.CertDays = -1
	}
	return nil
}

// AlertRule 是告警规则。
type AlertRule struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Metric    string  `json:"metric"` // cpu | mem | disk | offline | latency
	Op        string  `json:"op"`     // gt | lt
	Threshold float64 `json:"threshold"`
	Duration  int     `json:"duration"` // 持续秒数
	Severity  string  `json:"severity"` // info | warn | critical
	Enabled   bool    `json:"enabled"`
	// NodeIDs / Groups 为空表示对全部节点生效；非空时按「节点 ID 或分组」匹配。
	NodeIDs []string `json:"nodeIds,omitempty"`
	Groups  []string `json:"groups,omitempty"`
	// ServiceIDs 只对服务类指标（service_down / latency / loss / jitter /
	// cert_days）生效；为空表示全部服务。
	ServiceIDs []string `json:"serviceIds,omitempty"`
	// Channels 限定这条规则的通知渠道（NotifyConfig.Channels 里的 ID）。
	//
	// 为空表示"全部启用渠道"（与旧行为一致）。有值时不存在的 ID 会被忽略，
	// 若全部不可用则回退为全部渠道并在正文里说明 —— 告警宁可发错地方，
	// 也不能因为配置被删就静默消失。
	Channels []string `json:"channels,omitempty"`
}

// Alert 是一条告警实例。
type Alert struct {
	ID          string  `json:"id"`
	RuleID      string  `json:"ruleId"`
	RuleName    string  `json:"ruleName"`
	NodeID      string  `json:"nodeId"`
	NodeName    string  `json:"nodeName"`
	ServiceID   string  `json:"serviceId,omitempty"`
	ServiceName string  `json:"serviceName,omitempty"`
	Severity    string  `json:"severity"`
	Message     string  `json:"message"`
	Value       float64 `json:"value"`
	StartedAt   int64   `json:"startedAt"`
	ResolvedAt  int64   `json:"resolvedAt"` // 0 表示仍在持续
	// Metric 是触发该告警的规则指标，仅用于引擎内部判断（如离线告警合并），
	// 不进 JSON：它属于规则而不是告警实例，下发出去只会让前端多一个不可信的字段。
	Metric string `json:"-"`
}

// Renewal 是一条人工登记的续费记录，用于续费日历与成本回溯。
type Renewal struct {
	ID        string  `json:"id"`
	NodeID    string  `json:"nodeId"`
	At        string  `json:"at"` // YYYY-MM-DD，实际续费日期
	Amount    float64 `json:"amount"`
	Currency  string  `json:"currency"`
	Note      string  `json:"note,omitempty"`
	CreatedAt int64   `json:"createdAt"`
}

// APIToken 是范围化的自动化令牌。只保存 SHA-256 摘要，
// 明文仅在创建响应中出现一次。
type APIToken struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Prefix     string   `json:"prefix"`
	Hash       string   `json:"-"`
	Scopes     []string `json:"scopes"`
	Paths      []string `json:"paths,omitempty"` // 允许的 API 路径前缀；空表示不限
	CreatedAt  int64    `json:"createdAt"`
	LastUsedAt int64    `json:"lastUsedAt,omitempty"`
	// LastUsedIP 是最近一次使用该令牌的来源地址。
	// 令牌泄露时，"它从哪儿被用的"往往比"什么时候"更能定位问题。
	LastUsedIP string `json:"lastUsedIp,omitempty"`
	ExpiresAt  int64  `json:"expiresAt,omitempty"`
}

// HasScope 判断令牌是否包含指定权限。
func (t *APIToken) HasScope(scope string) bool {
	for _, s := range t.Scopes {
		if s == "admin" {
			return true
		}
		if s == scope {
			return true
		}
	}
	return false
}

// AllowsPath 判断令牌是否允许访问该路径。路径按段边界前缀匹配：
// /api/nodes/n1 放行 /api/nodes/n1 与 /api/nodes/n1/series，但不放行 /api/nodes/n10。
func (t *APIToken) AllowsPath(path string) bool {
	if len(t.Paths) == 0 {
		return true
	}
	for _, p := range t.Paths {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

// AuditEntry 是一条管理操作审计记录。
// 只记录操作元数据，不记录请求体，避免密码、令牌等敏感内容落盘。
type AuditEntry struct {
	ID     string `json:"id"`
	TS     int64  `json:"ts"`
	Action string `json:"action"`
	Actor  string `json:"actor"`
	Role   string `json:"role"` // session | api-token | admin-token | basic
	IP     string `json:"ip"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Event 是公开活动流中的一条事件（不含敏感字段）。
type Event struct {
	ID       string `json:"id"`
	TS       int64  `json:"ts"`
	Kind     string `json:"kind"` // alert_opened | alert_resolved | node_online | node_offline
	Level    string `json:"level"`
	NodeID   string `json:"nodeId,omitempty"`
	NodeName string `json:"nodeName,omitempty"`
	Text     string `json:"text"`
}

// 通知投递任务状态。
const (
	NotifyPending    = "pending"    // 等待投递或重试
	NotifySent       = "sent"       // 至少一个通道投递成功
	NotifyFailed     = "failed"     // 重试耗尽，仍需人工关注
	NotifySuppressed = "suppressed" // 静默或通知配置停用，未实际投递
)

// NotifyJob 是一条持久化的通知投递任务。
// 任务先落内存队列并由 Outbox 异步投递，重启后未完成任务会继续重试，
// 避免同步发送时的并发丢弃与进程退出丢通知。
type NotifyJob struct {
	ID       string `json:"id"`
	AlertID  string `json:"alertId,omitempty"`
	RuleID   string `json:"ruleId"`
	RuleName string `json:"ruleName"`
	NodeID   string `json:"nodeId"`
	NodeName string `json:"nodeName"`
	Severity string `json:"severity"`
	Resolved bool   `json:"resolved"`
	// Channels 是本次投递的目标渠道 ID（空=全部启用渠道）。
	Channels      []string `json:"channels,omitempty"`
	Title         string   `json:"title"`
	Body          string   `json:"body"`
	Value         float64  `json:"value"`
	Link          string   `json:"link"`
	EventTS       int64    `json:"eventTs"`
	CreatedAt     int64    `json:"createdAt"`
	Attempts      int      `json:"attempts"`
	NextAttemptAt int64    `json:"nextAttemptAt"`
	LastError     string   `json:"lastError,omitempty"`
	Status        string   `json:"status"`
	SentAt        int64    `json:"sentAt,omitempty"`
	// FinishedAt 是任务进入终态（sent/failed/suppressed）的时间。
	// 只有 SentAt 时，失败与静默的任务没有时间戳，面板无法回答
	// 「什么时候失败的」，而调用方一直在传这个时间。
	FinishedAt   int64  `json:"finishedAt,omitempty"`
	SuppressedBy string `json:"suppressedBy,omitempty"` // global 或规则 ID
}

// Silence 是一段通知静默期。Scope 为 "global" 或规则 ID。
// 静默只抑制通知投递，告警本身仍会正常记录与恢复。
type Silence struct {
	Scope     string `json:"scope"`
	Until     int64  `json:"until"`
	Reason    string `json:"reason,omitempty"`
	CreatedAt int64  `json:"createdAt"`
}

// NotifyStats 是通知队列与投递累计统计。
// Pending 为实时计算值，其余计数器跨重启保留。
type NotifyStats struct {
	Pending     int64 `json:"pending"`
	Sent        int64 `json:"sent"`
	Failed      int64 `json:"failed"`
	Retries     int64 `json:"retries"`
	Suppressed  int64 `json:"suppressed"`
	Rejected    int64 `json:"rejected"` // 队列满且无法清理时拒绝的任务数
	LastSuccess int64 `json:"lastSuccess"`
}

// ---------- 公开状态页 ----------

// 公告级别。与告警级别取值一致，便于前端复用同一套配色。
const (
	LevelInfo     = "info"
	LevelWarn     = "warn"
	LevelCritical = "critical"
)

// ValidLevel 判断公告/事件级别是否合法。
func ValidLevel(v string) bool {
	switch v {
	case LevelInfo, LevelWarn, LevelCritical:
		return true
	}
	return false
}

// StatusAnnouncement 是状态页顶部的置顶公告。
// 用于"已知问题正在处理"这类人工说明，与自动告警相互独立。
type StatusAnnouncement struct {
	Enabled   bool   `json:"enabled"`
	Level     string `json:"level"` // info | warn | critical
	Text      string `json:"text"`
	UpdatedAt int64  `json:"updatedAt"`
}

// StatusVisibility 控制哪些字段允许出现在**公开**状态页。
//
// 默认值全部为 false 的语义在这里不成立：未配置的字段按"不公开"处理最安全，
// 因此这里逐项显式列出，由 DefaultStatusPage 决定默认开放哪些。
// 主机名、内核版本、CPU 型号属于可用于指纹识别的信息，默认不公开。
type StatusVisibility struct {
	Metrics  bool `json:"metrics"`  // CPU / 内存 / 磁盘 使用率
	Load     bool `json:"load"`     // 负载 1/5/15
	Uptime   bool `json:"uptime"`   // 连续运行时间
	Traffic  bool `json:"traffic"`  // 实时网速与累计流量
	Procs    bool `json:"procs"`    // 进程数
	TCPConn  bool `json:"tcpConn"`  // TCP 连接数
	Swap     bool `json:"swap"`     // Swap 用量
	Hostname bool `json:"hostname"` // 真实主机名（可用于指纹识别）
	OS       bool `json:"os"`
	Kernel   bool `json:"kernel"`
	Arch     bool `json:"arch"`
	CPUModel bool `json:"cpuModel"`
	Cores    bool `json:"cores"`
	Virt     bool `json:"virt"`     // 虚拟化类型
	Region   bool `json:"region"`   // 地区
	Provider bool `json:"provider"` // 服务商
	Country  bool `json:"country"`  // 国家（用于公开页分布图）
	// 资产类字段默认关闭：价格与到期日属于内部经营信息。
	Billing bool `json:"billing"` // 续费价格与周期
	Expire  bool `json:"expire"`  // 到期日
	Quota   bool `json:"quota"`   // 流量配额与周期用量
	Tags    bool `json:"tags"`    // 节点标签
}

// StatusBlocks 控制公开页显示哪些区块。
type StatusBlocks struct {
	Summary    bool `json:"summary"`    // 顶部 KPI 汇总条
	Services   bool `json:"services"`   // 服务探测区
	Incidents  bool `json:"incidents"`  // 事件时间线
	History    bool `json:"history"`    // N 天可用率日柱
	NodeDetail bool `json:"nodeDetail"` // 允许点进节点详情页
	Map        bool `json:"map"`        // 国家/地区分布图
	Activity   bool `json:"activity"`   // 最近活动流
}

// StatusPage 是公开状态页的展示配置。
//
// 序列化时先填 DefaultStatusPage() 再反序列化：JSON 里不存在的键会保留默认值，
// 这样"新增字段"不会让老配置被静默读成 false。
type StatusPage struct {
	// Enabled 决定公开页是否对外提供。nil 与 false 都表示关闭：
	// 公开页会暴露节点名与在线状态，默认必须是关的（历史配置没有这个键，
	// 升级后一律视为关闭，需要显式打开）。
	Enabled *bool `json:"enabled,omitempty"`
	// Token 非空时，访问公开页与 /api/status* 必须带上它（?t= / Cookie / Bearer）。
	// 只做"知道链接"这一层门槛，不是账号体系；为空表示不校验。
	Token string `json:"token,omitempty"`
	// HiddenNodes 是逐台隐藏的节点 ID：既不进入列表，也不能按句柄单独查看。
	HiddenNodes []string `json:"hiddenNodes,omitempty"`
	// PublicNotes 是"节点 ID → 公开备注"，只出现在公开页；
	// 与内部的 Note 字段完全分离，避免把内部备注（含价格、账号信息）泄漏出去。
	PublicNotes  map[string]string  `json:"publicNotes,omitempty"`
	Title        string             `json:"title"`
	Announcement StatusAnnouncement `json:"announcement"`
	Visibility   StatusVisibility   `json:"visibility"`
	Blocks       StatusBlocks       `json:"blocks"`
	HistoryDays  int                `json:"historyDays"` // 可用率日柱天数：30 / 60 / 90
	// Sections 是公开页区块的展示顺序（从上到下）。
	// 未知键被丢弃、重复键去重、缺失键按默认顺序补在末尾 ——
	// 这样"新增区块"不会因为老配置里没有该键就从页面上消失。
	Sections []string `json:"sections"`
}

// IsEnabled 判断公开页是否开放。缺省（nil）视为关闭。
func (sp StatusPage) IsEnabled() bool { return sp.Enabled != nil && *sp.Enabled }

// HiddenSet 返回隐藏节点集合，便于 O(1) 判断。
func (sp StatusPage) HiddenSet() map[string]bool {
	if len(sp.HiddenNodes) == 0 {
		return nil
	}
	out := make(map[string]bool, len(sp.HiddenNodes))
	for _, id := range sp.HiddenNodes {
		if id = strings.TrimSpace(id); id != "" {
			out[id] = true
		}
	}
	return out
}

// PublicNote 返回某个节点的公开备注（没有则空串）。
func (sp StatusPage) PublicNote(nodeID string) string {
	if len(sp.PublicNotes) == 0 {
		return ""
	}
	return strings.TrimSpace(sp.PublicNotes[nodeID])
}

// StatusSectionKeys 是所有可排序区块的标识，顺序即默认展示顺序。
// 与前端 status.js 的区块渲染一一对应；改名会让已保存的顺序失效
// （NormalizeSections 会丢弃未知键并补齐默认项，页面仍可用）。
var StatusSectionKeys = []string{"nodes", "services", "map", "history", "incidents", "activity"}

// NormalizeSections 把任意输入修正为合法、无重复、覆盖全部区块的顺序。
func NormalizeSections(in []string) []string {
	known := make(map[string]bool, len(StatusSectionKeys))
	for _, k := range StatusSectionKeys {
		known[k] = true
	}
	seen := make(map[string]bool, len(StatusSectionKeys))
	out := make([]string, 0, len(StatusSectionKeys))
	for _, k := range in {
		k = strings.TrimSpace(k)
		if !known[k] || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	// 补齐缺失区块：老配置升级后不会丢模块
	for _, k := range StatusSectionKeys {
		if !seen[k] {
			out = append(out, k)
		}
	}
	return out
}

// DefaultStatusPage 返回默认配置。
// 默认只公开"对外可说"的聚合指标，主机指纹类字段一律关闭。
//
// Enabled 默认开启（v1.25.0 起的产品决策）：状态页是探针的核心卖点，
// 装好就该能看到，而不是先撞一个 404 再去翻文档找开关。脱敏保证
// （无 IP/价格/到期日/主机指纹）不受此影响；不想公开的实例用
// 「状态页 → 总开关」显式关闭，或配访问令牌变成私享链接。
// 显式关闭过的配置（enabled=false）升级后保持关闭。
func DefaultStatusPage() StatusPage {
	on := true
	return StatusPage{
		Enabled: &on,
		Title:   "服务状态",
		Visibility: StatusVisibility{
			Metrics: true, Load: true, Uptime: true,
			Traffic: true, Procs: false, TCPConn: false, Swap: false,
			Hostname: false, OS: false, Kernel: false, Arch: false,
			CPUModel: false, Cores: false, Virt: false,
			// ★ 地域默认不公开：它属于资产台账范畴（机群分布在哪些地区），
			// README 的脱敏原则只承诺展示名/分组/状态/指标/可用率。
			// 需要展示时由管理员在「状态页 → 展示配置」里显式开启。
			Region: false, Provider: false, Country: false,
		},
		Blocks: StatusBlocks{
			Summary: true, Services: true, Incidents: true,
			History: true, NodeDetail: true, Map: true, Activity: true,
		},
		HistoryDays: 90,
		Sections:    append([]string(nil), StatusSectionKeys...),
	}
}

// Normalize 把配置修正到合法取值：非法级别退回 info，
// 天数夹到 30–90，标题过长截断。任何非法输入都不能让公开页报错。
func (c *StatusPage) Normalize() {
	if c.Title == "" {
		c.Title = "服务状态"
	}
	if len([]rune(c.Title)) > 64 {
		c.Title = string([]rune(c.Title)[:64])
	}
	if !ValidLevel(c.Announcement.Level) {
		c.Announcement.Level = LevelInfo
	}
	if len([]rune(c.Announcement.Text)) > 1000 {
		c.Announcement.Text = string([]rune(c.Announcement.Text)[:1000])
	}
	if !c.Announcement.Enabled || c.Announcement.Text == "" {
		// 空公告必须整体失效：否则前端会渲染一个空的横幅占位块。
		c.Announcement.Enabled = false
	}
	switch c.HistoryDays {
	case 30, 60, 90:
	default:
		c.HistoryDays = 90
	}
	c.Sections = NormalizeSections(c.Sections)
}

// Maintenance 是一段计划维护窗口。
//
// 窗口内的节点：状态页标记为"维护中"，告警引擎不再对其产生新告警，
// 已存在的活跃告警以"计划维护"为由关闭 —— 否则计划内重启会刷一屏离线告警。
type Maintenance struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Reason    string   `json:"reason,omitempty"`
	NodeIDs   []string `json:"nodeIds,omitempty"` // 空表示全部节点
	StartAt   int64    `json:"startAt"`
	EndAt     int64    `json:"endAt"`
	CreatedAt int64    `json:"createdAt"`
}

// ActiveAt 判断窗口在 ts 时刻是否生效。
// EndAt <= StartAt 视为配置错误，按不生效处理（否则窗口永不结束）。
func (m *Maintenance) ActiveAt(ts int64) bool {
	if m == nil || m.EndAt <= m.StartAt {
		return false
	}
	return ts >= m.StartAt && ts < m.EndAt
}

// Covers 判断窗口是否覆盖指定节点。NodeIDs 为空表示全局。
// nil 窗口与 ActiveAt 保持一致，视为不覆盖任何节点。
func (m *Maintenance) Covers(nodeID string) bool {
	if m == nil {
		return false
	}
	if len(m.NodeIDs) == 0 {
		return true
	}
	for _, id := range m.NodeIDs {
		if id == nodeID {
			return true
		}
	}
	return false
}

// IncidentUpdate 是事件进展时间线上的一条公开更新。
// Status 用固定枚举，前端据此着色并推导"已恢复"。
type IncidentUpdate struct {
	TS     int64  `json:"ts"`
	Status string `json:"status"` // investigating | identified | monitoring | resolved
	Text   string `json:"text"`
}

// 事件进展状态取值。
const (
	IncidentInvestigating = "investigating"
	IncidentIdentified    = "identified"
	IncidentMonitoring    = "monitoring"
	IncidentResolved      = "resolved"
)

// NormalizeIncidentStatus 把未知状态归到 investigating。
func NormalizeIncidentStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case IncidentIdentified:
		return IncidentIdentified
	case IncidentMonitoring:
		return IncidentMonitoring
	case IncidentResolved:
		return IncidentResolved
	default:
		return IncidentInvestigating
	}
}

// Incident 是一条人工登记的事件（事故/变更/公告）。
// 与告警的区别：告警由规则自动生成，事件由管理员手工撰写，
// 用于状态页的"事件时间线"里说明那些没有触发告警但用户应该知道的事。
type Incident struct {
	ID        string           `json:"id"`
	Title     string           `json:"title"`
	Body      string           `json:"body,omitempty"`
	Level     string           `json:"level"`             // info | warn | critical
	NodeIDs   []string         `json:"nodeIds,omitempty"` // 受影响的节点，空表示全局
	StartAt   int64            `json:"startAt"`
	EndAt     int64            `json:"endAt"` // 0 表示仍在进行
	CreatedAt int64            `json:"createdAt"`
	Updates   []IncidentUpdate `json:"updates,omitempty"`
}

// AgentUpgradeBatch 是一次 Agent 升级的清单与基线。
// 服务端不远程执行升级，只记录批次创建时各节点的版本，
// 由后续上报的 AgentVersion 推导每台机器是否已完成升级。
type AgentUpgradeBatch struct {
	ID            string            `json:"id"`
	TargetVersion string            `json:"targetVersion"`
	Note          string            `json:"note,omitempty"`
	CreatedAt     int64             `json:"createdAt"`
	NodeIDs       []string          `json:"nodeIds"`
	Before        map[string]string `json:"before"` // nodeID -> 创建时版本
}

// AgentUpgradeTarget 是批次详情里的一台节点。
type AgentUpgradeTarget struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Before  string `json:"before"`
	Current string `json:"current"`
	State   string `json:"state"` // done | pending | missing
}

// ---------- 以下是直接下发给前端的视图模型 ----------

// 节点状态。online/stale/offline 由最后上报时间推导；
// no_data 表示节点已登记但 Agent 从未上报（与"曾在线后失联"不同）。
const (
	NodeStateNoData  = "no_data"
	NodeStateOnline  = "online"
	NodeStateStale   = "stale"
	NodeStateOffline = "offline"
)

// NodeView 是节点列表项，合并台账、最新指标与状态。
//
// 这里刻意**不内嵌 Node**，而是逐个复制台账字段：内嵌会把 Token 一起带进
// 这个结构，只靠 `json:"-"` 挡住 JSON 序列化。一旦有模板渲染、日志打印、
// %+v 调试输出或未来的 msgpack/protobuf 编码路径，上报令牌就会随视图外泄。
// 结构性隔离比标签约束可靠。
type NodeView struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Provider string  `json:"provider"`
	Region   string  `json:"region"`
	Group    string  `json:"group"`
	Note     string  `json:"note"`
	ExpireAt string  `json:"expireAt"`
	Price    float64 `json:"price"`
	Currency string  `json:"currency"`
	Weight   int     `json:"weight"`
	Country  string  `json:"country,omitempty"`
	Favorite bool    `json:"favorite,omitempty"`
	// AlertExempt 台账级告警豁免（节点编辑/批量操作设置）。
	AlertExempt bool         `json:"alertExempt,omitempty"`
	Traffic     TrafficUsage `json:"traffic"`
	Tags        []string     `json:"tags,omitempty"`

	Host     HostInfo `json:"host"`
	Latest   Metrics  `json:"latest"`
	Online   bool     `json:"online"`
	State    string   `json:"state"`
	LastSeen int64    `json:"lastSeen"`
	Alerts   int      `json:"alerts"`
	Spark    []Point  `json:"spark"` // 首页迷你曲线

	AgentVersion  string `json:"agentVersion,omitempty"`
	AgentOutdated bool   `json:"agentOutdated,omitempty"`

	// RateIn/RateOut 是最近两点的网卡速率（B/s）。nodeViews 在构造
	// spark 的同一次序列读取里顺带求差，不额外访问历史。
	RateIn  uint64 `json:"rateIn,omitempty"`
	RateOut uint64 `json:"rateOut,omitempty"`
}

// NodeMeta 从台账条目复制视图所需的字段，调用方再补齐监控状态。
func NodeMeta(n *Node) NodeView {
	if n == nil {
		return NodeView{}
	}
	return NodeView{
		ID: n.ID, Name: n.Name, Provider: n.Provider, Region: n.Region,
		Group: n.Group, Note: n.Note, ExpireAt: n.ExpireAt,
		Price: n.Price, Currency: n.Currency, Weight: n.Weight,
		Country: n.Country, Favorite: n.Favorite, AlertExempt: n.AlertExempt,
		Tags: append([]string(nil), n.Tags...),
	}
}

// Point 是一个时序采样点（用于前端绘图）。
type Point struct {
	T int64   `json:"t"`
	V float64 `json:"v"`
}

// HistoryPoint 是降采样后的历史点，用于 7 天/30 天/365 天查询。
// 聚合只保留平均值、CPU 峰值、样本数与网络增量，不复制完整 Metrics。
type HistoryPoint struct {
	TS         int64   `json:"ts"`
	CPUPct     float64 `json:"cpuPct"` // 桶内平均
	CPUMax     float64 `json:"cpuMax"` // 桶内峰值
	MemPct     float64 `json:"memPct"`
	MemUsed    uint64  `json:"memUsed"`
	MemTotal   uint64  `json:"memTotal"`
	DiskPct    float64 `json:"diskPct"`
	DiskUsed   uint64  `json:"diskUsed"`
	DiskTotal  uint64  `json:"diskTotal"`
	Load1      float64 `json:"load1"`
	NetRxDelta uint64  `json:"netRxDelta"`
	NetTxDelta uint64  `json:"netTxDelta"`
	Count      uint32  `json:"count"`
}

// Overview 是总览页的聚合数据。
type Overview struct {
	Nodes       int     `json:"nodes"`
	Online      int     `json:"online"`
	Stale       int     `json:"stale"`
	Offline     int     `json:"offline"`
	NoData      int     `json:"noData"`
	ActiveAlert int     `json:"activeAlert"`
	AvgCPU      float64 `json:"avgCpu"`
	AvgMem      float64 `json:"avgMem"`
	Services    int     `json:"services"`
	SvcDown     int     `json:"svcDown"`
	TrafficIn   uint64  `json:"trafficIn"`
	TrafficOut  uint64  `json:"trafficOut"`
	MonthlyCost float64 `json:"monthlyCost"`
	// RateIn/RateOut 是各节点最近两点速率的合计（B/s），
	// 与 TrafficIn/TrafficOut（累计计数器合计）是两种口径。
	RateIn  uint64 `json:"rateIn"`
	RateOut uint64 `json:"rateOut"`
	// PeriodUsed 是各节点本计费周期用量（按各自口径合成的 Used）之和；
	// PeriodQuota 是配额字节数之和（未设配额的节点贡献 0）。
	PeriodUsed  uint64 `json:"periodUsed"`
	PeriodQuota uint64 `json:"periodQuota"`
	// RemainingValue 是未到期资产的剩余价值合计（USD）：年付价 × 剩余天数 / 365。
	// 与 MonthlyCost 同口径（CNY 按 cnyRate 折算）；未登记到期日或已过期不计入。
	RemainingValue float64    `json:"remainingValue"`
	Expiring       []NodeView `json:"expiring"` // 30 天内到期
}

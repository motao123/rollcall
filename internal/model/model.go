// Package model 定义所有跨组件共享的数据结构。
// 设计约束：只包含数据结构与纯函数，不引入任何外部依赖。
package model

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
	// AgentVersion 是 Agent 最近一次上报的版本，由上报请求头写入，
	// 台账 API 不能修改；用于面板提示落后的 Agent。
	AgentVersion string `json:"agentVersion,omitempty"`
	Token        string `json:"-"` // 上报凭证，永不下发给前端
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
}

// Report 是 Agent → Server 的单向上报载荷。
// 注意：协议中不存在任何服务端 → Agent 的指令通道。
type Report struct {
	NodeID  string   `json:"nodeId"`
	Host    HostInfo `json:"host"`
	Metrics Metrics  `json:"metrics"`
}

// Service 是一个服务探测目标（HTTP / TCP）。
type Service struct {
	ID       string `json:"id"`
	NodeID   string `json:"nodeId"` // 归组到某个节点，用于展示
	Name     string `json:"name"`
	Type     string `json:"type"` // http | tcp
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
}

// Alert 是一条告警实例。
type Alert struct {
	ID         string  `json:"id"`
	RuleID     string  `json:"ruleId"`
	RuleName   string  `json:"ruleName"`
	NodeID     string  `json:"nodeId"`
	NodeName   string  `json:"nodeName"`
	Severity   string  `json:"severity"`
	Message    string  `json:"message"`
	Value      float64 `json:"value"`
	StartedAt  int64   `json:"startedAt"`
	ResolvedAt int64   `json:"resolvedAt"` // 0 表示仍在持续
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
	ID            string  `json:"id"`
	AlertID       string  `json:"alertId,omitempty"`
	RuleID        string  `json:"ruleId"`
	RuleName      string  `json:"ruleName"`
	NodeID        string  `json:"nodeId"`
	NodeName      string  `json:"nodeName"`
	Severity      string  `json:"severity"`
	Resolved      bool    `json:"resolved"`
	Title         string  `json:"title"`
	Body          string  `json:"body"`
	Value         float64 `json:"value"`
	Link          string  `json:"link"`
	EventTS       int64   `json:"eventTs"`
	CreatedAt     int64   `json:"createdAt"`
	Attempts      int     `json:"attempts"`
	NextAttemptAt int64   `json:"nextAttemptAt"`
	LastError     string  `json:"lastError,omitempty"`
	Status        string  `json:"status"`
	SentAt        int64   `json:"sentAt,omitempty"`
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
}

// StatusBlocks 控制公开页显示哪些区块。
type StatusBlocks struct {
	Summary    bool `json:"summary"`    // 顶部 KPI 汇总条
	Services   bool `json:"services"`   // 服务探测区
	Incidents  bool `json:"incidents"`  // 事件时间线
	History    bool `json:"history"`    // N 天可用率日柱
	NodeDetail bool `json:"nodeDetail"` // 允许点进节点详情页
}

// StatusPage 是公开状态页的展示配置。
//
// 序列化时先填 DefaultStatusPage() 再反序列化：JSON 里不存在的键会保留默认值，
// 这样"新增字段"不会让老配置被静默读成 false。
type StatusPage struct {
	Title        string             `json:"title"`
	Announcement StatusAnnouncement `json:"announcement"`
	Visibility   StatusVisibility   `json:"visibility"`
	Blocks       StatusBlocks       `json:"blocks"`
	HistoryDays  int                `json:"historyDays"` // 可用率日柱天数：30 / 60 / 90
}

// DefaultStatusPage 返回默认配置。
// 默认只公开"对外可说"的聚合指标，主机指纹类字段一律关闭。
func DefaultStatusPage() StatusPage {
	return StatusPage{
		Title: "服务状态",
		Visibility: StatusVisibility{
			Metrics: true, Load: true, Uptime: true,
			Traffic: true, Procs: false, TCPConn: false, Swap: false,
			Hostname: false, OS: false, Kernel: false, Arch: false,
			CPUModel: false, Cores: false, Virt: false,
			// ★ 地域默认不公开：它属于资产台账范畴（机群分布在哪些地区），
			// README 的脱敏原则只承诺展示名/分组/状态/指标/可用率。
			// 需要展示时由管理员在「状态页 → 展示配置」里显式开启。
			Region: false, Provider: false,
		},
		Blocks: StatusBlocks{
			Summary: true, Services: true, Incidents: true,
			History: true, NodeDetail: true,
		},
		HistoryDays: 90,
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

// Incident 是一条人工登记的事件（事故/变更/公告）。
// 与告警的区别：告警由规则自动生成，事件由管理员手工撰写，
// 用于状态页的"事件时间线"里说明那些没有触发告警但用户应该知道的事。
type Incident struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Body      string   `json:"body,omitempty"`
	Level     string   `json:"level"`             // info | warn | critical
	NodeIDs   []string `json:"nodeIds,omitempty"` // 受影响的节点，空表示全局
	StartAt   int64    `json:"startAt"`
	EndAt     int64    `json:"endAt"` // 0 表示仍在进行
	CreatedAt int64    `json:"createdAt"`
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

	Host     HostInfo `json:"host"`
	Latest   Metrics  `json:"latest"`
	Online   bool     `json:"online"`
	State    string   `json:"state"`
	LastSeen int64    `json:"lastSeen"`
	Alerts   int      `json:"alerts"`
	Spark    []Point  `json:"spark"` // 首页迷你曲线

	AgentVersion  string `json:"agentVersion,omitempty"`
	AgentOutdated bool   `json:"agentOutdated,omitempty"`
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
	Nodes       int        `json:"nodes"`
	Online      int        `json:"online"`
	Stale       int        `json:"stale"`
	Offline     int        `json:"offline"`
	NoData      int        `json:"noData"`
	ActiveAlert int        `json:"activeAlert"`
	AvgCPU      float64    `json:"avgCpu"`
	AvgMem      float64    `json:"avgMem"`
	Services    int        `json:"services"`
	SvcDown     int        `json:"svcDown"`
	TrafficIn   uint64     `json:"trafficIn"`
	TrafficOut  uint64     `json:"trafficOut"`
	MonthlyCost float64    `json:"monthlyCost"`
	Expiring    []NodeView `json:"expiring"` // 30 天内到期
}

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
	Token    string  `json:"-"`        // 上报凭证，永不下发给前端
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

// ---------- 以下是直接下发给前端的视图模型 ----------

// NodeView 是节点列表项，合并台账、最新指标与状态。
type NodeView struct {
	Node
	Host     HostInfo `json:"host"`
	Latest   Metrics  `json:"latest"`
	Online   bool     `json:"online"`
	LastSeen int64    `json:"lastSeen"`
	Alerts   int      `json:"alerts"`
	Spark    []Point  `json:"spark"` // 首页迷你曲线
}

// Point 是一个时序采样点（用于前端绘图）。
type Point struct {
	T int64   `json:"t"`
	V float64 `json:"v"`
}

// Overview 是总览页的聚合数据。
type Overview struct {
	Nodes       int        `json:"nodes"`
	Online      int        `json:"online"`
	Offline     int        `json:"offline"`
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

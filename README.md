# Rollcall — 轻量机群监控平台

> 曾用名 **Probe**（2026-10-18 改名）。`docs/` 下的两份历史报告仍使用旧名，为当时事实记录，保留原貌。

当前版本 **v1.9.0**（`./rollcall-server -version` 可查）。

一个**零外部依赖**的自托管服务器监控平台。融合 Nezha 的「服务探测能力」与 Komari 的「极简部署与清爽界面」，并按"Agent 只读"的安全原则重新设计。

```
┌────────────────┐                          ┌─────────────────┐
│ rollcall-agent │                          │ rollcall-server │
│  （只读采集）  │      ──▶ 单向 POST       │（Go + 内嵌前端）│
└────────────────┘                          └─────────────────┘
                                                    │
                                            ┌────────────────┐
                                            │ Web UI（原生） │
                                            │    7 个视图    │
                                            └────────────────┘
```

---

## 一、为什么做这个项目

现有两个主流方案各有取舍：

| | Nezha | Komari |
|---|---|---|
| 监控粒度 | 高（服务探测/证书/告警联动） | 基础（CPU/内存/磁盘/延迟） |
| 运维能力 | **强**（WebSSH、文件管理、任务下发） | 无 |
| 部署复杂度 | 中（gRPC + 反代区分多种协议） | 低（单容器、无数据库） |
| 安全面 | **大**（Agent 可执行任意命令） | 小（仅上报） |
| 代码可审计性 | 低（仓库 14.2MB） | 中 |
| 资产台账 | 无 | 无 |

**Rollcall 的定位：保留前者"看得清"的能力与后者"装得快"的体验，砍掉前者"能被执行"的危险面，并补上两者都缺的资产台账。**

核心取舍是一句话：

> **能力缺失比代码自主更安全。**

Agent 里不 import `os/exec`、不 `net.Listen`，攻击者即使拿到面板也无处下发命令 —— 攻击面从"任意 RCE"降到"只能伪造指标数据"。
唯一的代码替换路径是可选的自更新：只接受本机配置的签名清单，面板与服务端都无法触发。

可用以下方式验证这个承诺：

```bash
# ① 依赖表里没有 os/exec —— 最快最可靠的证明
go list -deps ./cmd/agent | grep -x 'os/exec'      # 期望：无输出

# ② 符号表检查（必须用未剥离的二进制，见下方警告）
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/agent-full ./cmd/agent
go tool nm /tmp/agent-full | wc -l                              # 期望：数千
go tool nm /tmp/agent-full | grep -cE 'os/exec\.|exec\.Cmd'     # 期望：0
go tool nm /tmp/agent-full | grep -cE 'net\.Listen|TCPListener' # 期望：0

# ③ 对照组：server 必须有 Listen，否则说明 grep 模式写错了
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/server-full ./cmd/server
go tool nm /tmp/server-full | grep -cE 'net\.Listen|TCPListener' # 期望：>0
```

> **⚠ 两个会让验证变成"假通过"的坑**（实测踩过）：
>
> 1. **不要 grep 源码**。`collect_linux.go` 的文档注释里本身就写着 `os/exec`、`net.Listen` 这些字样，直接 `grep -r "os/exec" ./internal/collect/` 必然命中注释，无法区分注释与真实调用。要看依赖表和符号表。
> 2. **`go tool nm` 必须用未剥离的二进制**。`make linux` 带 `-ldflags "-s -w"` 会剥掉符号表，此时 nm 报 `no symbol section`，任何 grep 计数都恒为 0 —— 看起来"通过"，其实什么都没验证到。所以先用不带 ldflags 的 `go build` 单独构建一份。

---

## 二、核心功能

### 1. 节点监控（融合）
- **Nezha 式指标维度**：CPU、内存、Swap、磁盘、负载（1/5/15）、TCP 连接数、进程数、运行时长、网络收发
- **Komari 式轻量实现**：无数据库、单二进制、启动即用
- 每个节点一张卡片：三色资源条 + 迷你 CPU 曲线 + 区域/流量/运行时长
- 节点详情：五张时序图（CPU / 内存 / 磁盘 / 网络速率 / 负载），支持 1h / 6h / 24h / 7d / 30d / 365d 切换；
  长范围自动使用 1m / 5m / 1h 聚合，不会一次返回数十万原始点

### 2. 服务探测（Nezha 强项）
- 协议：HTTP(S) / TCP
- 指标：可用性、延迟、HTTP 状态码、**TLS 证书剩余天数**（不足一天按 1 计，`0`=已过期，`-1`=非 TLS）
- 24 小时可用率统计 + 最近 2 小时延迟迷你曲线
- **异常服务自动置顶**，一眼看到哪里坏了
- 探测由**服务端发起**，不经 Agent —— 单个 Agent 沦陷无法污染其他节点的服务状态
- **默认拒绝内网目标**（回环/RFC1918/链路本地/metadata/CGNAT）：确有内网监控需求时显式加
  `-allow-private-probes`；HTTPS 证书默认严格校验，自签名目标需显式加 `-insecure-probe-tls`
- HTTP 跳转逐跳重新校验地址，3xx 不再默认视为健康；TCP 目标必须是合法的 `host:port`

### 3. 告警与通知（融合三者）
- 规则维度：`offline` / `cpu` / `mem` / `disk` / `load`
- 支持 `gt` / `lt` 比较与 **持续时长抑制**（避免抖动误报）
- 三级严重度：`info` / `warn` / `critical`，可按阈值过滤
- 告警自动恢复（指标回落即置为已恢复）
- 默认规则开箱即用：失联 3 分钟、CPU>90% 持续 5 分钟、内存>90%、磁盘>85%
- **规则可在界面增删改**：阈值、持续时间、级别、启停全部可调
  （`offline` 规则自动隐藏阈值输入框）；服务端做完整校验
  （metric 白名单、阈值区间 1–100、duration 0–86400、severity 白名单）
- **规则可定向**：按节点或分组限定生效范围（`nodeIds` / `groups`，界面可选节点、填分组），
  留空表示对所有节点生效；规则收紧后，超出范围的旧告警自动关闭
- **节点令牌轮换**：设置页「节点管理 → 接入」可一键轮换，旧令牌立即失效，页面同步给出新的 agent.json

**通知通道（10 个，参考 certimate / dnsmgr / domain-admin 设计）**

| 通道 | 类型标识 | 说明 |
|---|---|---|
| 企业微信群机器人 | `wecom` | Webhook |
| 钉钉群机器人 | `dingtalk` | Webhook + 可选加签 |
| 飞书群机器人 | `lark` | Webhook + 可选签名 |
| Telegram | `telegram` | Bot Token + Chat ID |
| Bark（iOS） | `bark` | 设备 Key，可自建服务端 |
| Server酱 | `serverchan` | SendKey |
| PushPlus | `pushplus` | Token，支持群组推送 |
| Gotify（自建） | `gotify` | 服务地址 + Token + 优先级 |
| 邮件 | `email` | SMTP，465 隐式 TLS / 587 STARTTLS |
| 通用 Webhook | `webhook` | JSON POST，可带 `X-Rollcall-Secret` |

- **可配置多个同类型通道**（例如两个不同的 Telegram 群），每个通道独立启停
- **逐通道测试**：配置阶段可单独发测试消息；错误只回传状态码/平台错误码，不回显远端响应体
- **出站安全**：不跟随 3xx（避免令牌被转发）、拒绝 metadata/链路本地地址、限制响应大小；
  通知目标**默认拒绝**回环/私网/CGNAT，自建 Gotify / Bark / Webhook 位于内网时需显式加
  `-allow-private-notify`；错误信息会剥离 URL，避免 Telegram Bot Token 等凭据进入日志
- **响应体校验**：企业微信/钉钉/飞书等出错时 HTTP 仍为 200，
  必须解析 `errcode`/`code` 才能发现真实失败 —— 已全部处理
- SMTP 默认强制 TLS：465 隐式 TLS，587/25 必须 STARTTLS；不支持 STARTTLS 时拒绝明文发送凭据
- 通知内容为「主题 + 正文」两段式，含节点、规则、当前值、级别、时间、面板链接
- 支持重复提醒间隔（0 = 只通知一次）
- **持久化 Outbox**：告警只负责入队，投递由 8 个有界 worker 异步完成；任务随快照落盘，
  重启后继续投递。失败按 30s → 2m → 10m → 30m 退避，最多 5 次，
  耗尽后保留为 `failed` 并写入日志，不再静默丢弃
- **部分成功语义**：任一通道送达即视为已送达，避免为补发失败通道向成功通道重复刷屏；
  逐通道错误保留在任务 `lastError` 中
- **通知静默**：支持全局或单规则静默（1 小时 / 6 小时 / 24 小时 / 7 天，可填备注）。
  静默期间告警仍正常记录与恢复，仅拦截外发；到期自动恢复
- 队列深度、送达/失败/重试/静默拦截/队列满拒绝与最近成功时间均可在告警页与 `/metrics` 查看
- **队列可运维**：设置页列出最近 100 条任务（状态、尝试次数、最近错误），失败或被静默拦截的任务
  可在修复通道后手动重试，重试会重置尝试次数并立即唤醒调度器
- **变更留痕**：规则停用、删除或收紧定向范围导致活跃告警被关闭时，会补发恢复通知，不再静默消失

**通知插件（可选，显式开启）**

内置通道之外，可用 `-notify-plugins <目录>` 加载本地插件：

- 目录顶层的 `*.json` 清单声明通道类型、展示名、配置字段与相对可执行文件；
  未指定该参数时，服务端没有任何执行外部程序的路径
- 投递时把通道配置与消息以 JSON 写入插件 stdin，**不经过 shell**；子进程环境完全重建，
  不继承服务端环境变量（避免管理令牌泄露），每次调用硬超时 1–30 秒，输出有上限
- 插件类型不能覆盖内置通道；清单与命令都限制在插件目录内，拒绝符号链接越界，
  命令必须是普通可执行文件
- 插件通道沿用现有通知页的编辑、脱敏与逐通道测试；修改清单后需重启服务端
- 插件在本机以服务端用户身份运行，属于**受信任的本地扩展**，不是沙箱；
  只应安装自己审计过的脚本

### 6. 外观主题（受限沙箱）

- 设置页可配置明暗模式、浅色/深色强调色、圆角（0–24px）与紧凑密度
- 服务端只接受固定字段（`mode` / `accent` / `accentDark` / `radius` / `compact`），
  未知字段与非法颜色直接拒绝；**永远不接受 CSS 文本**
- 前端只把这些值映射到白名单 CSS 变量；公开状态页同步应用，且不新增任何数据暴露
- 主题随设置持久化；`GET /api/theme` 公开下发（仅样式），
  管理端用 `GET/PUT /api/admin/theme` 读写

### 7. Agent 签名自更新（可选，默认关闭）

- Agent 从配置中的**清单地址主动拉取**更新，不开放入站端口，服务端不参与、也无法下发更新指令
- 清单是 Ed25519 签名信封，包含版本、平台资产 URL、SHA-256 与大小；签名、哈希或大小不符即拒绝
- 更新包必须是小端 ELF 且架构匹配，下载到同目录后原子替换，保留 `.prev` 备份
- 新版本连续 3 次启动仍未完成一次成功上报时自动回滚；失败版本 7 天内不再自动尝试
- 手动回滚 `rollcall-agent -rollback`；立即检查 `rollcall-agent -update-now`
- 自更新需要 Agent 对其二进制所在目录有写权限，systemd 加固下还需放行该目录
  （见运维手册）；默认安装路径 `/usr/local/bin` 归 root 所有，因此默认不会自动更新

### 4. 资产台账（两者都没有）
这是 40 台规模最现实的痛点：**忘续费 = 直接丢机器**。

- 字段：服务商、地域、分组、到期日、年付价、币种、备注
- 30 天内到期自动进入「即将到期」列表（7 天内标红）
- 月度成本自动折算（年付 ÷ 12，CNY 按汇率折算为 USD 聚合）
- 节点卡片与台账共用同一份数据，不重复录入

### 5. 公开状态页（免登录）

访问 `/status` 得到一个**完全公开、只读**的状态页，可直接挂到域名下。
信息架构参考主流探针（Kokoro、ss.akz 等）：**总体健康 → 筛选 → 节点卡片 → 节点详情**，
但数据面严格受后台的「可见性」与「区块」开关约束。

- **总览**
  - 健康环 + 总体结论（全部正常 / 部分异常 / 等待接入）
  - KPI 汇总：在线节点、24h 平均可用率、平均 CPU/内存/磁盘、实时下行、服务健康、活跃告警
  - 搜索（名称/分组/地区/系统）+ 状态筛选 + 分组下拉
  - 节点卡片：状态徽标与数据延迟/离线的"最后上报"提示、CPU/内存/磁盘进度条
    （按 75%/90% 变色）、实时上下行与累计流量、CPU 迷你趋势、**48 段 24h 可用性色块**
- **节点详情**（`#/n/<hid>`）
  - 资源环（CPU/内存/磁盘）、24h 可用率、负载/运行时间/进程/TCP/Swap/实时网速
  - 主机信息（按开关展示主机名、系统、内核、架构、CPU 型号、核心数、虚拟化）
  - 性能趋势：**24 小时 / 7 天 / 30 天**可切换；24h 用原始点，7d/30d 用 5m/1h 聚合层，
    服务端降采样到 ≤240 点，避免一次请求传输数万个点
  - 服务延迟：曲线**只画成功探测**（失败请求的 0ms 不会伪装成低延迟），
    失败率与平均延迟单独标注
  - 每日可用率色柱
- **交互与外观**
  - 每 15 秒轮询，页面隐藏时暂停；列表采用增量 DOM 更新，数值/进度条做平滑过渡，
    搜索框与筛选状态不会被刷新打断
  - 深色优先，自动跟随系统，也可手动切换「跟随系统 / 浅色 / 深色」（localStorage 记忆）
  - 桌面多列卡片、平板两列、手机单列；`prefers-reduced-motion` 下关闭动画
  - 自包含（内联 CSS + 同源 `status.js`），零外部资源请求；脚本可被 CSP 拦截时
    12 秒后显示纯 CSS 降级提示

**公开 API**（均无需登录，响应支持 gzip）：

| 接口 | 说明 |
|---|---|
| `GET /api/status` | 站点名、节点列表、服务探测、每日可用率、事件、可见性与区块配置 |
| `GET /api/status/node/<hid>?range=24h\|7d\|30d` | 单节点详情与趋势；只接受不透明句柄 |
| `GET /api/status/incidents` | 事件时间线 |
| `GET /api/theme` | 主题配置（模式/强调色/圆角/紧凑密度） |

**技术选型（公开页 / 探针面板）**：

| 层 | 选择 | 原因 |
|---|---|---|
| 服务端 | Go 标准库 `net/http`，JSON API | 无框架、单二进制；与现有 API/鉴权/存储共用一套进程 |
| 传输 | 15s 轮询 + `Cache-Control: no-store` | 探针场景下轮询最稳、可穿透代理；无需维护 SSE/WebSocket 连接 |
| 前端 | 原生 ES5 风格 JS + 事件委托 | 不引入 React/Vue；无构建链，`go:embed` 直接打包 |
| 图表 | 手绘内联 SVG（折线/面积/双曲线/环/色块） | 零图表库依赖，可精确控制深色主题与降采样后的渲染成本 |
| 样式 | CSS 变量 + Grid/Flex + `backdrop-filter` | 一处定义深浅色；移动端仅靠媒体查询即可重排 |
| 刷新 | 增量 DOM patch + View Transition（详情跳转） | 保留搜索焦点、进度条过渡和滚动位置 |

**脱敏原则**（公开页可被任何人访问）：
不含 IP、价格、到期日、备注、上报令牌。主机名、服务商、地区、系统、内核、架构、
CPU 型号、核心数、虚拟化、进程数、TCP、Swap 等默认关闭，需在「状态页 → 展示配置」
逐项开启；节点在公开页上只以**不透明句柄**（`hid`，由服务端盐值派生，不可逆推回内部 ID）标识，
内部节点 ID 永不下发；`/api/status/node/<hid>` 也只接受句柄，用内部 ID 访问返回 404。
默认公开：展示名、分组、在线状态、三项使用率、负载、运行时长、实时流量、24h 可用率。
服务探测只公开名称、状态、可用率、延迟与失败率，不公开 Target。

**节点状态语义**（仪表盘与公开页一致）：

| 状态 | 条件 | 展示 |
|---|---|---|
| `online` | 最后上报 ≤ 120 秒 | 在线 |
| `stale` | 最后上报 120–300 秒 | 数据延迟（琥珀色） |
| `offline` | 最后上报 > 300 秒 | 离线 |
| `no_data` | 节点已登记但 Agent 从未上报 | 待接入；指标显示 `—` 而非 0 |

告警引擎的 `offline` 规则仍以 120 秒为判定窗口，不受展示层 `stale` 影响。

**与面板登录的关系**：`/` 管理后台采用**账号 + 密码**登录（支持用户名或邮箱），
密码仅以 PBKDF2-HMAC-SHA256 哈希保存在服务端 `data/admin-user.json`（0600），
登录态是 HMAC 签名的 HttpOnly 会话 Cookie（默认 7 天，`SameSite=Strict`）。
`/status` 与 `/api/status` **始终可匿名访问**（这是公开页的设计意图）。
管理写接口与 CSV 导出接受会话 Cookie（写操作需带 `X-Requested-With`）或 `X-Admin-Token`；
Agent 上报只认节点令牌。

---

## 三、数据采集与展示

### 采集方式：只读、单向、无指令通道

Agent 默认每 30 秒读取以下文件（`interval` 可调，最大 60 秒），**不做任何其他操作**：

| 指标 | 来源 |
|---|---|
| CPU 使用率 | `/proc/stat` 两次差值 |
| 内存 / Swap | `/proc/meminfo`（用 `MemAvailable` 算真实可用） |
| 负载 / 进程数 | `/proc/loadavg` |
| 磁盘 | `statfs("/")` |
| 网络收发 | `/proc/net/dev`（排除 lo / veth / docker） |
| TCP 连接数 | `/proc/net/tcp{,6}` 中状态为 `01`（ESTABLISHED） |
| 运行时长 | `/proc/uptime` |
| 主机信息 | `/etc/os-release`、`/proc/cpuinfo`、`/proc/sys/kernel/osrelease` |

**上传协议**：`POST /api/report`，`X-Node-Token` 认证，JSON 体。服务端用**自身时间**校正时间戳，避免 Agent 时钟漂移污染时序。

### 存储：环形缓冲 + 分层历史 + JSON 快照

对齐 Komari 的"无数据库"理念，同时保证重启不丢历史：

- **原始层**：每节点一个定长环形缓冲（2880 点 = 24h @30s），支撑 1h/6h/24h 查询
- **聚合层**：1m × 7d、5m × 30d、1h × 365d 三级桶（默认），支撑 7d/30d/365d 查询；
  桶内保存平均值、CPU 峰值、网络增量与样本数，内存约 70MB @ 40 节点满保留期；
  三级保留期可分别用 `-history-1m-retention` / `-history-5m-retention` /
  `-history-1h-retention` 调整（须满足 1m ≤ 5m ≤ 1h，且为对应聚合窗口的整数倍）
- 聚合桶从 schema 2 起记录内存/磁盘**绝对用量总和**，扩容后不会因
  「最新总量 × 历史百分比」产生系统性偏差；旧桶自动回退到原百分比换算
- 默认每 60 秒把状态快照写入 `data/state.json`（`-save-interval` 可调，10s–1h；gzip 压缩 JSON，先写 `.tmp` 再 rename，保证原子性）
- 聚合历史每 5 分钟单独写入 `data/history.json`，与主快照解耦；主快照体积不随保留期增长
  （history schema 当前为 2；旧 schema 1 可直接载入并自动补齐用量总和字段）
- 启动时自动恢复；兼容旧版本写出的明文 JSON，首次保存后自动转为 gzip
- 历史缺失或损坏时自动用 24h 原始点回填，损坏文件隔离为 `history.json.corrupt-<时间>`；
  正常重启只回放上次落盘之后的原始点，不会重复累计
- 快照带 `schemaVersion`（当前为 3）：更高版本的快照会被隔离为 `state.json.newer-<时间>` 并拒绝载入，
  避免新结构被旧程序静默读成零值；v2 新增通知队列、投递统计与静默期，
  v3 仅提升版本号（历史聚合写入独立文件），旧版 v1/v2/无版本快照仍可直接载入
- 快照损坏会隔离为 `state.json.corrupt-<时间>` 并拒绝启动，
  而不是静默以空库运行（否则全部节点与令牌会凭空消失）

**为什么不用 SQLite？** 40 台规模下，环形缓冲 + JSON 已足够，且少一个依赖就少一个需要审计和升级的组件。若未来规模翻数倍，替换 `internal/store` 即可，上层无感。

### 展示：服务端聚合 + 前端自绘

- 后端按视图聚合（`/api/overview` 直接给出 KPI，前端不做计算）
- **图表全部用内联 SVG 手绘**，无 ECharts/uPlot 等第三方库 —— 少 200KB+ 依赖，且完全可控
- 前端每 20–30 秒静默刷新，页面隐藏时暂停（省电、省请求）

---

## 四、界面设计

### 布局结构

```
┌──────────┬────────────────────────────────────────┐
│          │  顶栏：标题 / 搜索 / 实时 / 刷新 / 账号菜单 │
│  侧边栏   ├────────────────────────────────────────┤
│  导航     │  KPI 行（6 项关键指标）                 │
│  主题     │  正在告警（仅异常时出现）                │
│          │  节点卡片网格                            │
│          │  即将到期表格                            │
└──────────┴────────────────────────────────────────┘
```

七个视图：**概览 / 节点 / 服务 / 告警 / 台账 / 状态页 / 设置**，左侧导航切换，节点详情走 `#/nodes/{id}`。
未登录时整页切换为账号密码登录表单；登录后顶栏右上角提供账号菜单与退出登录。

### 视觉风格

- **单一强调色**（钢蓝 `#2f5d8a`），不做渐变堆叠
- 状态色严格语义化：绿=正常、琥珀=警告、红=严重
- 1px 描边 + 极轻阴影，信息密度优先于装饰
- **数字全部用等宽字体 + `tabular-nums`**，纵向对齐（监控面板的刚需）
- 无图标字体、无外部字体、无 CDN —— 全部内联 SVG 与系统字体栈
- CSS 变量驱动主题，深浅色切换无闪烁

### 响应式适配

| 断点 | 行为 |
|---|---|
| > 1080px | 完整侧边栏（232px）+ 多列网格 |
| ≤ 1080px | 侧边栏收成 66px 图标栏；详情页两栏转单栏 |
| ≤ 720px | 侧边栏变**底部标签栏**；KPI 两列；节点卡片单列 |
| 触屏 | 点击区域 ≥ 40px，禁用 hover-only 交互 |

### 交互细节

- 骨架屏加载，避免布局跳动
- 节点卡片 hover 微抬 + 边框提亮
- 键盘快捷键：`/` 搜索、`T` 主题、`R` 刷新、`G` 回概览、`Esc` 失焦
- 主题选择持久化到 `localStorage`，首次跟随系统偏好
- 遵守 `prefers-reduced-motion`，自动关闭动画
- 所有错误态有明确文案（数据加载失败会提示"服务端是否启动"）

---

## 五、快速开始

### 前置条件

- 生产：Linux + systemd
- 构建机：**Go 1.24+**（`go.mod` 声明 `go 1.24.0`；低版本会触发 `GOTOOLCHAIN` 自动下载工具链，
  离线构建机会直接失败）、git、curl
- 生产端口默认只监听回环，对外请用反向代理或内置 TLS

### 1. 获取源码并构建

```bash
git clone https://github.com/motao123/rollcall.git
cd rollcall
go build -o rollcall-server ./cmd/server
go build -o rollcall-agent ./cmd/agent

# 交叉编译示例
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o rollcall-server ./cmd/server
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o rollcall-agent ./cmd/agent
```

> 项目零外部依赖，`go build` 不需要联网。`make linux` 只是上面的封装，
> 使用前需要构建机安装 `make`。需要生成自更新签名清单时，在**签名机**上执行
> `make rollsign` 构建 `./rollsign`（不要复制到被监控节点）。
>
> 版本号通过 ldflags 注入：`make linux VERSION=v1.9.0`。直接 `go build` 时
> 版本为 `dev`；`./rollcall-server -version` 与 `/metrics` 的 `rollcall_build_info` 均可查看。
>
> 并发回归测试用 `go test -race ./...`（需要 gcc/CGO）；CI 已单独配置 `race` job。
>
> 在 Windows 上跑 `go test ./...` 会**跳过**几个依赖 POSIX 权限位的用例
> （插件可执行位、`0600` 文件权限、原子替换回滚），这是平台差异而非失败；
> 对应的安全断言在 Linux/CI 上照常执行。

### 2. 预览 UI（可选）

```bash
./rollcall-server -addr 127.0.0.1:9188 -demo
```

打开 `http://127.0.0.1:9188`。演示模式使用临时数据目录，退出后自动删除，
不会污染生产 `state.json`。

### 3. 安装服务端（systemd）

```bash
sudo useradd --system --home /opt/rollcall --shell /usr/sbin/nologin rollcall
sudo install -d -o rollcall -g rollcall -m 0750 /opt/rollcall /opt/rollcall/data
sudo install -m 0755 rollcall-server /opt/rollcall/rollcall-server
sudo install -m 0644 deploy/rollcall-server.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now rollcall-server
curl -fsS http://127.0.0.1:19080/healthz
```

`deploy/rollcall-server.service` 默认**不开启**私网探测/通知。确需监控本机服务或向内网
webhook 推送时，用 systemd drop-in 追加 `-allow-private-probes` / `-allow-private-notify`
（写法见 [`docs/operations.md`](docs/operations.md)），不要直接修改模板默认值。

> 日常运维（备份恢复、升级回滚、令牌轮换、故障排查）见 [`docs/operations.md`](docs/operations.md)。

> 服务端收到 SIGINT/SIGTERM 后会停止接收新请求、等待在途请求结束并立即保存快照，
> systemd 重启不会丢失最近一次自动保存（60 秒）之后的数据。

### 4. 首次登录

首次启动时会在 `data/admin-user.json` 创建唯一管理员账号。两种初始化方式任选：

```bash
# 方式 A（推荐）：首次启动前通过环境变量指定用户名、邮箱和初始密码
sudo install -d -m 0750 /etc/rollcall
sudo sh -c 'umask 077; cat > /etc/rollcall/server.env' <<'EOF'
ROLLCALL_ADMIN_USER=admin
ROLLCALL_ADMIN_EMAIL=you@example.com
ROLLCALL_ADMIN_PASSWORD=<至少 12 位的强密码>
EOF
sudo systemctl edit rollcall-server
# 在打开的编辑器中写入：
# [Service]
# EnvironmentFile=-/etc/rollcall/server.env
sudo systemctl restart rollcall-server

# 方式 B：不提供密码，服务端随机生成 24 位初始密码并只打印一次
sudo journalctl -u rollcall-server -n 50 | grep '初始管理员密码'
```

打开 `http://127.0.0.1:19080`（或反代域名），输入用户名/邮箱与密码登录。
登录后可在「设置 → 管理员账号」查看当前账号并退出登录。

> 环境变量只用于**首次创建**账号。建议登录后立即按下方重置流程换成自己的强密码，
> 然后删除 `/etc/rollcall/server.env` 中的密码行（单元使用 `EnvironmentFile=-`，
> 文件缺失不影响启动），避免明文密码长期留在磁盘。

> 密码只在创建或显式重置时写入。正常重启不会用环境变量覆盖已保存的密码。
> 忘记密码时执行一次（该命令只改密码后退出，不会启动第二个服务）：
> ```bash
> sudo -u rollcall env ROLLCALL_ADMIN_PASSWORD='<新密码>' \
>   /opt/rollcall/rollcall-server -data /opt/rollcall/data -reset-admin-password
> sudo systemctl restart rollcall-server
> ```

对外访问请用 SSH 隧道或反向代理，不要直接把 19080 暴露到公网。

### 5. 接入一台节点

```bash
# 在控制端创建节点（返回 id 与 token）
TOKEN=$(sudo cat /opt/rollcall/data/admin.token)
curl -s -X POST http://127.0.0.1:19080/api/admin/nodes \
  -H "X-Admin-Token: $TOKEN" -H "Content-Type: application/json" \
  -d '{"name":"节点名","provider":"racknerd","region":"美国","group":"prod"}'
```

把 `rollcall-agent` 和 `deploy/rollcall-agent.service` 上传到目标节点后执行：

```bash
# 1. 安装二进制与 systemd 单元（在仓库目录执行）
install -m 0755 rollcall-agent /usr/local/bin/rollcall-agent
install -m 0644 deploy/rollcall-agent.service /etc/systemd/system/

# 2. 创建专用低权限用户（Agent 只需读 /proc，不需要 root）
useradd --system --no-create-home --shell /usr/sbin/nologin rollcall-agent
install -d -m 0750 -o root -g rollcall-agent /etc/rollcall

# 3. 写入配置（server/nodeId/token 用上一步返回值）
#    server 必须是 https://...；仅本机回环可用 http://127.0.0.1:19080
cat >/etc/rollcall/agent.json <<'EOF'
{
  "server": "https://rollcall.example.com",
  "nodeId": "n_xxxx",
  "token": "<48位上报令牌>",
  "interval": 30
}
EOF
chown root:rollcall-agent /etc/rollcall/agent.json
chmod 0640 /etc/rollcall/agent.json

# 3.1 可选：启用签名自更新（默认关闭；不需要可跳过）
#     在 agent.json 中增加 update 块后再启动：
# {
#   "update": {
#     "enabled": true,
#     "manifestUrl": "https://updates.example.com/agent/manifest.json",
#     "publicKey": "<base64 Ed25519 公钥>",
#     "checkIntervalHours": 6
#   }
# }
# manifestUrl 必须是 HTTPS（本机回环可用 HTTP）；publicKey 由 `rollsign keygen` 生成，
# 私钥只保留在签名机。自更新还要求 Agent 对二进制目录有写权限，见运维手册。

# 4. 启动
systemctl daemon-reload
systemctl enable --now rollcall-agent

# 5. 验证一次上报（失败会以非 0 退出，约 2 秒）
runuser -u rollcall-agent -- \
  /usr/local/bin/rollcall-agent -config /etc/rollcall/agent.json -once
```

> `interval` 单位为秒，默认 30，**最大 60**：服务端 120 秒未收到上报即判定离线，
> 间隔过大会把一次抖动误报为失联。超出上限时 Agent 会拒绝启动并给出明确提示。
>
> `name` 会显示在公开状态页，请使用展示名，**不要填主机名或 IP**。
> 后续可在「设置」页编辑节点台账（`PUT /api/admin/nodes/{id}`），编辑不会轮换令牌。

> Agent 默认拒绝向**非本机**的明文 HTTP 服务端上报（节点令牌会裸奔）。
> 远程节点必须使用 `https://`；仅本机回环或明确接受风险时才加 `-allow-insecure`。
> Agent 不跟随 3xx，确保 `X-Node-Token` 不会被转发到其他主机。

> **自更新权限**：Agent 以低权限用户运行时，必须对**二进制所在目录**有写权限才能原子替换自身；
> 默认的 `/usr/local/bin` 归 root 所有，未做额外配置时自更新只会失败并记录日志，不影响上报。
> 需要自更新时，建议把二进制放到专用目录（目录属主 `rollcall-agent`）：
>
> ```bash
> install -d -o rollcall-agent -g rollcall-agent -m 0750 /opt/rollcall-agent
> install -m 0755 -o rollcall-agent -g rollcall-agent \
>   rollcall-agent /opt/rollcall-agent/rollcall-agent
> ```
>
> 并在 systemd drop-in 中把 `ExecStart` 指向新路径，同时放行写权限：
>
> ```ini
> [Service]
> ExecStart=
> ExecStart=/opt/rollcall-agent/rollcall-agent -config /etc/rollcall/agent.json
> ReadWritePaths=/opt/rollcall-agent
> ```
>
> `-rollback` 恢复 `.prev` 后自动重启；`.update-pending` 记录待验证版本与启动次数，
> 首次成功上报后自动删除。签名与清单生成见 `docs/operations.md`。

### 5.1 一条命令接入（可选，需 `-agent-dir`）

手动步骤共 5 步、容易漏配。把 Agent 二进制放到服务端目录并加 `-agent-dir`，
面板的「接入」弹窗就会给出一条可直接粘贴的命令：

```bash
# 控制端：准备分发目录（用 make linux 的产物）
sudo install -d -o rollcall -g rollcall -m 0755 /opt/rollcall/agent
sudo install -m 0755 dist/rollcall-agent-linux-amd64 dist/rollcall-agent-linux-arm64 /opt/rollcall/agent/
# 在 rollcall-server.service 的 ExecStart 追加 -agent-dir /opt/rollcall/agent
# （drop-in 写法见 docs/operations.md；改完 systemctl daemon-reload && systemctl restart rollcall-server）
```

```bash
# 目标节点：一条命令
curl -fsSL https://你的面板/agent/install.sh | sh -s -- https://你的面板 <node-id> <token>
```

脚本会自动识别架构、创建 `rollcall-agent` 低权限用户（与 `deploy/rollcall-agent.service`
姿态一致）、写入 0640 配置、生成 systemd 单元；没有 systemd 的容器里退回后台常驻。
未配置 `-agent-dir` 时 `/agent/*` 返回 404，仍可按上面的手动步骤安装。

> 脚本本身不含任何密钥（node id / token 由命令行参数传入），因此可以公开下载与审计。
> 但它以 `curl | sh` 方式执行，**只在 HTTPS 或 SSH 隧道下使用**：明文 HTTP 下中间人
> 可以替换脚本或二进制。同理，Agent 二进制目前只做 `-version` 可执行性校验，不校验签名。

> **端口约定**：全文统一用 **19080** 作为生产端口（与 `deploy/rollcall-server.service` 一致）。
>
> **安全默认**：监听非回环地址时，明文 HTTP 会**直接拒绝启动**（必须配置 TLS 或显式加 `-allow-insecure`）。
> 面板始终要求账号密码登录，因此不再需要为启动额外配置 Basic Auth。
> 三种对外方式：
> 1. 反向代理到 `127.0.0.1:19080`（推荐，由反代终止 TLS）；
> 2. 内置 TLS：`-addr 0.0.0.0:19080 -tls-cert fullchain.pem -tls-key privkey.pem`；
> 3. 仅限可信内网：显式加 `-allow-insecure` 接受明文 HTTP（不推荐）。
>
> `-allow-public` 仅为兼容旧部署保留，已不再影响启动。

完整参数：

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-addr` | `127.0.0.1:19080` | 监听地址；非回环需满足上面的安全条件 |
| `-demo` | false | 载入 15 台演示数据，使用临时数据目录，不发起真实探测 |
| `-panel-link` | 空 | 面板对外地址（用于告警链接和 Agent 接入指引） |
| `-tls-cert` / `-tls-key` | 空 | 同时提供则启用 HTTPS |
| `-auth` | 空 | 兼容用 `user:password` Basic Auth（面板 UI 不使用；保留给旧集成） |
| `-allow-public` | false | 已废弃，仅为兼容旧启动参数保留 |
| `-allow-insecure` | false | 允许非回环地址使用明文 HTTP |
| `-allow-private-probes` | false | 允许 HTTP/TCP 探测内网与回环目标 |
| `-allow-private-notify` | false | 允许通知通道访问内网/回环目标（自建 Gotify/Bark/Webhook） |
| `-notify-plugins` | 空 | 通知插件目录；显式指定后加载顶层 `*.json` 清单 |
| `-agent-dir` | 空 | Agent 二进制分发目录（内含 `rollcall-agent-linux-{amd64,arm64}`），启用面板里的「一条命令接入」；未配置时 `/agent/*` 返回 404 |
| `-trusted-proxies` | 空 | 可信反向代理 CIDR（逗号分隔，如 `127.0.0.1/32,10.0.0.0/8`）；留空只信任回环。放在反代后面时必须配置，否则日志与限速会把所有访客当成同一个 IP |
| `-insecure-probe-tls` | false | 探测 HTTPS 时跳过证书校验（自签名目标） |
| `-probe-interval` | `10s` | 探测调度粒度；每个服务按自身 `interval` 执行 |
| `-save-interval` | `60s` | 自动保存快照间隔（10s–1h）；退出时还会强制保存一次 |
| `-history-1m-retention` | `168h` | 1 分钟聚合保留期（≥1m、为 60s 整数倍） |
| `-history-5m-retention` | `720h` | 5 分钟聚合保留期（≥5m、为 300s 整数倍） |
| `-history-1h-retention` | `8760h` | 1 小时聚合保留期（≥1h、为 3600s 整数倍） |
| `-cny-rate` | `7.2` | CNY 折算 USD 的汇率（仅用于成本聚合展示） |
| `-admin-user` | `admin` / `ROLLCALL_ADMIN_USER` | 面板管理员用户名（2–32 位字母/数字/点/下划线/连字符） |
| `-admin-email` | 空 / `ROLLCALL_ADMIN_EMAIL` | 可选邮箱，设置后可用邮箱登录 |
| `-admin-password` | 空 / `ROLLCALL_ADMIN_PASSWORD` | 初始密码（至少 12 位）；仅在首次创建或重置时生效，留空则随机生成并打印一次 |
| `-reset-admin-password` | false | 用上面的密码重置管理员密码后退出；重启服务后旧会话失效 |
| `-admin-token` | 自动生成 | 管理 API 令牌（供自动化/监控使用），显式指定时至少 16 字符 |
| `-data` | `./data` | 数据目录（state.json / history.json / admin.token / admin-user.json / session.key） |
| `-version` | false | 打印版本号后退出 |

### 管理 API

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/admin/nodes` | 列出节点（含上报令牌） |
| POST | `/api/admin/nodes` | 创建节点，服务端生成 id 与令牌 |
| GET | `/api/admin/nodes/{id}` | 查看单个节点（含上报令牌，用于重新接入） |
| PUT | `/api/admin/nodes/{id}` | 编辑台账字段（保留 ID 与上报令牌） |
| POST | `/api/admin/nodes/{id}/rotate-token` | 轮换上报令牌（旧令牌立即失效） |
| DELETE | `/api/admin/nodes/{id}` | 删除节点 |
| GET/POST | `/api/admin/services` | 列出 / 创建探测目标（`enabled` 默认 true） |
| DELETE | `/api/admin/services/{id}` | 删除探测目标 |
| GET/POST | `/api/admin/settings` | 读取（脱敏）/ 保存通知配置 |
| GET/PUT | `/api/admin/theme` | 读取 / 保存外观主题（固定字段，未知字段拒绝） |
| POST | `/api/admin/test-notify` | 发送测试通知，可带 `{"channelId":"..."}` 只测单个 |
| GET/POST | `/api/admin/rules` | 列出 / 创建告警规则 |
| GET/PUT/DELETE | `/api/admin/rules/{id}` | 查看 / 修改 / 删除单条规则 |
| GET | `/api/admin/rule-meta` | 规则表单元数据（指标/级别/时长选项，需令牌） |
| GET | `/api/admin/silences` | 当前生效的通知静默 |
| PUT/DELETE | `/api/admin/silences/{global\|ruleId}` | 设置（`{"minutes":60,"reason":"..."}`）/ 取消静默 |
| GET | `/api/admin/notify/jobs` | 通知队列最近 100 条任务与累计统计 |
| POST | `/api/admin/notify/jobs/{id}/retry` | 手动重试失败/被拦截任务（重置尝试次数） |
| GET/PUT | `/api/admin/status-page` | 读取 / 保存公开页配置（标题、公告、展示天数、字段可见性、区块开关） |
| GET/POST | `/api/admin/maintenance` | 列出 / 新建维护窗口（`{title,reason,nodeIds,startAt,endAt}`，省略时间为「现在起 60 分钟」） |
| DELETE | `/api/admin/maintenance/{id}` | 删除维护窗口；窗口内该节点的告警自动抑制并在窗口结束后恢复评估 |
| GET/POST | `/api/admin/incidents` | 列出 / 新建人工事件（`{title,body,level,nodeIds,startAt,endAt}`） |
| DELETE | `/api/admin/incidents/{id}` | 删除人工事件 |
| POST | `/api/admin/nodes/batch` | 批量改台账：`{ids,action,value}`，`action` ∈ `group\|provider\|region\|expire\|currency\|delete`。任一行 ID 不存在则整批拒绝，避免改一半 |

公开接口（无需任何凭据）：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/status` | 公开状态页 HTML |
| GET | `/api/status` | 公开状态页数据（严格脱敏，节点只带 `hid` 句柄） |
| GET | `/api/status/node/<hid>` | 单节点详情（指标/曲线/逐日可用率），受展示配置裁剪；区块关闭或句柄无效均 404 |
| GET | `/api/status/incidents` | 事件时间线（告警自动事件 + 人工登记事件合并后按时间倒序） |
| GET | `/api/theme` | 公开外观主题（仅固定样式字段） |
| GET | `/status.js` | 公开状态页脚本 |
| GET | `/healthz` | 健康检查 |

管理接口与 CSV 导出接受两种凭据：浏览器会话 Cookie（写操作需带
`X-Requested-With: XMLHttpRequest`）或请求头 `X-Admin-Token`（供自动化使用）。
管理令牌首次启动时自动生成到 `data/admin.token`（0600），也可用 `-admin-token` 显式指定。
`/api/` 下的 JSON 与 CSV 响应在客户端支持时自动 gzip 压缩：365 天曲线从约 2.5MB 降到约 300KB，慢速链路下也能正常打开。

所有路由的鉴权级别集中声明在服务端路由表中（`public` / `login` / `node-token` /
`readonly` / `admin` / `metrics`）；新增路由若漏填级别，服务端会在启动时直接失败，
不再依赖易漏改的路径白名单。`/status.js` 单独登记为公开，因此公开状态页始终可正常加载。

账号登录端点：

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/auth/login` | `{"identifier":"用户名或邮箱","password":"..."}`；成功写入 HttpOnly 会话 Cookie |
| GET | `/api/auth/session` | 返回 `{authenticated,user?,expiresAt?}`；未登录也是 200 |
| POST | `/api/auth/logout` | 清除会话 Cookie（幂等） |

仪表盘只读接口（包含主机名、内核、服务商、价格、到期日等资产信息，不再匿名开放）：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/overview` | KPI 汇总 |
| GET | `/api/nodes` | 节点列表与最新指标 |
| GET | `/api/nodes/{id}` | 节点详情 |
| GET | `/api/nodes/{id}/series?range=1h\|6h\|24h\|7d\|30d\|365d` | 历史时序（长范围自动降采样） |
| GET | `/api/services` | 服务探测状态 |
| GET | `/api/alerts` | 告警历史 |
| GET | `/api/rules` | 告警规则 |
| GET | `/api/assets` | 资产台账视图 |

鉴权：登录后的会话 Cookie；自动化可继续使用 `X-Admin-Token`；启用 `-auth` 时也接受面板 Basic 凭据。
会话失效时前端会显示账号密码登录页，并停止自动刷新，避免持续 401 触发限速。

### 对外暴露与面板集成

控制端默认只监听 `127.0.0.1`（安全默认）。两种对外方式：

**A. SSH 隧道（零配置）**

```bash
ssh -N -L 19080:127.0.0.1:19080 root@<控制端 IP>
# 浏览器访问 http://127.0.0.1:19080
```

**B. 面板反向代理（推荐长期使用）**

*宝塔*：网站 → 添加站点 → 站点设置 → 反向代理 → 目标 `http://127.0.0.1:19080`，SSL 可一键申请。

*1Panel*：网站 → 创建网站 → 反向代理类型 → 代理地址填 `http://127.0.0.1:19080`。

**C. 内置 TLS（无需反代）**

```bash
# 在 rollcall-server.service 的 ExecStart 追加：
-addr 0.0.0.0:19080 \
  -tls-cert /etc/rollcall/fullchain.pem -tls-key /etc/rollcall/privkey.pem
```

> 非回环绑定必须配置 TLS；明文 HTTP 会被拒绝启动。
> 若确实要用明文 HTTP 暴露到可信内网，必须显式加 `-allow-insecure`。
> 面板始终要求账号密码登录，`-allow-public` 已不再影响启动。

登录失败、管理令牌失败与 Basic Auth 失败会按来源 IP 分别限速（5 分钟内 10 次失败封禁 10 分钟），
并记录 `ip/method/path` 审计日志。会话 Cookie 带 `HttpOnly`、`SameSite=Strict`，
TLS（或 `X-Forwarded-Proto: https`）下自动加 `Secure`；会话签名密钥持久化在
`data/session.key`（0600），重启不会让已登录用户掉线。

针对面板环境的设计取舍：不依赖 Docker（避免与面板容器管理冲突）、文件不侵入 `/www/server` 或 `/opt/1panel`、端口避开 888/7870/8888、仅绑 localhost 天然适配反代。

---

## 六、项目结构

```
rollcall/
├── cmd/
│   ├── server/main.go       服务端入口
│   ├── agent/main.go        只读 Agent 入口
│   └── rollsign/main.go     更新清单签名工具（keygen / sign）
├── internal/
│   ├── model/               数据结构（无依赖）
│   ├── store/               环形缓冲 + 分层聚合 + JSON 快照
│   ├── collect/             /proc 采集（linux / 非 linux 两套）
│   ├── netguard/            出站地址策略（SSRF 防护，探测/通知共用）
│   ├── probe/               HTTP/TCP 探测
│   ├── alert/               告警引擎
│   ├── auth/                管理员账号（PBKDF2 哈希）与会话签名
│   ├── notify/              通知通道 + 本地插件加载/执行
│   ├── outbox/              持久化通知队列
│   ├── selfupdate/          Ed25519 清单校验、下载、原子替换与回滚
│   ├── theme/               受限主题模型与校验
│   ├── version/             构建版本号（ldflags 注入）
│   └── web/
│       ├── web.go           管理/公开 API + 内嵌静态资源
│       ├── statuspub.go     公开状态页数据：脱敏、可见性、区间聚合、服务延迟
│       ├── statuspage.go    公开页 HTML/CSS 外壳（深色优先、响应式、CSP 兼容）
│       ├── auth.go          登录 / 会话 / 退出端点
│       └── dist/
│           ├── index.html   管理后台外壳
│           ├── app.js       管理后台 SPA
│           ├── style.css    管理后台样式
│           └── status.js    公开页渲染：路由、增量刷新、手绘 SVG 图表
├── deploy/                  systemd 单元与配置样例
├── docs/operations.md       备份、升级、插件、自更新与故障排查
├── .github/workflows/ci.yml gofmt / vet / test / -race / 交叉编译
└── LICENSE                  MIT
```

---

## 七、安全设计要点

| 风险 | 本项目的处理 |
|---|---|
| Agent 被用作 RAT | 不 import `os/exec`；唯一执行路径是 `syscall.Exec` 重启**已签名**的新版本自身 |
| Agent 被远程控制 | 协议单向（仅 POST 上报），服务端不连 Agent，也不能触发自更新 |
| Agent 开放端口 | 不 `net.Listen`，无入站攻击面 |
| Agent 更新被投毒 | 自更新默认关闭；Ed25519 签名清单 + SHA-256/大小校验 + ELF 架构检查；不跟随重定向 |
| 主题编辑器注入 | 只接受固定字段与 `#rrggbb`，拒绝未知字段；前端仅映射白名单 CSS 变量，永不渲染 CSS 文本 |
| 通知插件越权 | 需本机 `-notify-plugins` 显式开启；清单/命令限制在插件目录，拒绝符号链接逃逸；无 shell，环境变量重建，1–30s 超时 |
| 上报伪造 | 每节点独立 token，**常量时间比较**；服务端时间戳校正 |
| 请求体攻击 | `MaxBytesReader` 限制 64KB；`DisallowUnknownFields` |
| XSS / 点击劫持 | 前端全量 `esc()` 转义；CSP `default-src 'self'` + `frame-ancestors 'none'`；`X-Frame-Options: DENY` |
| 响应头 | `X-Content-Type-Options`、`Referrer-Policy`、HSTS（反代终止 TLS 时同样生效） |
| 凭证外泄 | `Node.Token` 标记 `json:"-"`；管理令牌只走请求头；会话凭证放 HttpOnly Cookie；密码只存 PBKDF2 哈希 |
| 误暴露到公网 | 默认只绑回环；非回环必须配置 TLS 或显式确认明文风险，否则拒绝启动 |
| 暴力破解 | 登录失败按 IP 独立分桶（5 分钟内 10 次失败封禁 10 分钟）；失败请求写审计日志 |
| CSRF | 会话 Cookie `SameSite=Strict`；会话写操作额外要求 `X-Requested-With` 自定义头 |
| SSRF（探测） | 拨号时校验解析后的 IP；默认拒绝回环/私网/链路本地/metadata/CGNAT；跳转逐跳复检 |
| SSRF（通知） | 不跟随重定向；始终拒绝 metadata/链路本地；自托管允许私网；不回显远端响应体 |
| 探测 TLS 降级 | 默认严格校验证书；跳过校验必须显式 `-insecure-probe-tls` |
| Agent 传输窃听 | 非本机服务端强制 HTTPS（或显式 `-allow-insecure`）；不跟随 3xx |
| 时序数据被污染 | 服务端覆盖 Agent 时间戳；时间倒退的样本直接丢弃 |
| 快照损坏 | 自动隔离为 `state.json.corrupt-<时间>` 并拒绝以空库启动，避免令牌/台账静默丢失 |

**明确不做的事**（这是设计选择，不是缺陷）：

- ✘ 不提供 Web 终端 / 文件管理器 / 远程命令 —— 需要时请用标准 SSH
- ✘ 不提供多租户与工单审批 —— 个人/小团队场景不需要
- ✘ 不做 PromQL 级深度可观测 —— 需要时上 Prometheus

---

## 八、已知限制

1. **未实现** ICMP 探测（需 raw socket 权限；HTTP/TCP 已覆盖绝大多数场景）
2. 长历史依赖本地分层聚合（默认 1h 层 365 天）；更长期归档或多实例仍需外接 TSDB
3. 单实例单进程，无水平扩展设计（40 台规模绰绰有余）
4. 无多用户与权限体系（单一管理员账号，适合个人/小团队）
5. Agent 自更新仅支持 Linux，且要求对二进制目录有写权限；服务端升级仍由运维执行
6. 主题只有固定字段，通知插件是本机受信任扩展而非沙箱；两者都需要管理权限才能配置

---

## 九、后续路线

| 阶段 | 内容 |
|---|---|
| ~~P1~~ | ~~节点管理 API + 前端管理界面~~ ✅ |
| ~~P2~~ | ~~告警通知（10 通道 + 逐通道测试）~~ ✅ |
| ~~P3~~ | ~~告警规则界面编辑（增删改 + 启停 + 服务端校验）~~ ✅ |
| ~~P4~~ | ~~公开状态页 `/status`（免登录 + 48 段 24h 色块 + 严格脱敏）~~ ✅ |
| ~~P5~~ | ~~数据导出（CSV + Prometheus exposition）~~ ✅ |
| ~~P6~~ | ~~通知可靠性：持久化 Outbox + 重试退避 + 全局/规则静默 + 队列指标~~ ✅ |
| ~~P7~~ | ~~外观主题沙箱（固定字段 + 白名单 CSS 变量）~~ ✅ |
| ~~P8~~ | ~~通知插件机制（本地清单、无 shell、环境/超时隔离）~~ ✅ |
| ~~P9~~ | ~~Agent 签名自更新 + 自动/手动回滚~~ ✅ |

**P5 明细**：

*CSV 导出*（需登录会话或管理令牌，设置页有按钮，也可直接调 API）

| 端点 | 内容 |
|---|---|
| `GET /api/export/assets.csv` | 资产台账 + 当前状态 + 月摊成本合计 |
| `GET /api/export/alerts.csv` | 告警历史（含开始/恢复时间、级别、说明） |
| `GET /api/export/metrics.csv?node=<id>&range=1h\|6h\|24h\|7d\|30d\|365d` | 单节点采样；1h/6h/24h 为原始 18 列，长范围为桶均值/增量 15 列 |

CSV 带 UTF-8 BOM，Excel 打开中文不乱码；文件名含时间戳。

*Prometheus 端点* `GET /metrics`

鉴权：`X-Admin-Token` 头，或 Basic Auth（用户名任意、**密码为管理令牌**）；
启用 `-auth` 时也接受面板 Basic 凭据。`/metrics` 不接受浏览器会话 Cookie
（Prometheus 无法携带），抓取请使用管理令牌。`/metrics` 独立校验，不受 Basic 前置拦截影响。
抓取配置：

```yaml
scrape_configs:
  - job_name: rollcall
    static_configs: [{ targets: ["host:19080"] }]
    basic_auth: { username: rollcall, password: "<admin.token 内容>" }  # username 任意，密码填令牌
```

暴露的指标族（节点/服务指标带 `id`/`name` 标签）：
`rollcall_build_info`、`rollcall_nodes_total/online/stale/offline/no_data`、`rollcall_alerts_active`、
`rollcall_services_total/down`、`rollcall_node_state{state=...}`、`rollcall_node_{online,cpu_percent,memory_percent,
disk_percent,load1/5/15,uptime_seconds,tcp_connections,processes,
network_receive_bytes_total,network_transmit_bytes_total,
last_report_age_seconds}`、`rollcall_service_{up,latency_milliseconds,
certificate_days_remaining,uptime_percent_24h}`

落盘与历史分层指标：
`rollcall_save_total`、`rollcall_save_failures_total`、`rollcall_save_duration_seconds`、
`rollcall_save_last_success_timestamp_seconds`、
`rollcall_history_points{id,name,resolution}`（1m/5m/1h 各级别保留的桶数）

通知队列指标：
`rollcall_notify_queue_pending`、`rollcall_notify_sent_total`、
`rollcall_notify_failed_total`、`rollcall_notify_retries_total`、
`rollcall_notify_suppressed_total`、`rollcall_notify_rejected_total`、
`rollcall_notify_last_success_timestamp_seconds`、`rollcall_notify_silences_active`

---

## 十、实际部署验证

已在 Ubuntu 22.04 / AMD EPYC 16 核 / 31GB 环境完成控制端 + 被控端同机部署验证（v1.9.0）：

| 项目 | 结果 |
|---|---|
| 服务状态 | rollcall-server / rollcall-agent 均 v1.9.0、active + enabled |
| 公开状态页 | `/status` 匿名可访问；13 个节点实测在线；1440px 三列 / 390px 单列均无横向溢出；浅色、深色、跟随系统切换正常 |
| 公开 API | `/api/status`、`/api/status/node/<hid>`、`/api/theme` 匿名 200；用内部节点 ID 访问详情返回 404；匿名 `/api/nodes` 401；`/metrics` 凭管理令牌 200 |
| 公开页脱敏 | 默认不下发地区、服务商、主机名、系统、内核、架构、CPU 型号、核心数、虚拟化、进程数、TCP、Swap；`/status` 与 `/api/status` 无内部 ID |
| 详情趋势 | 24h / 7d / 30d 切换正常，服务端降采样上限 240 点；空载 `0.00` 负载仍会下发而不是被当作空字段 |
| 增量刷新 | 15 秒轮询保持搜索输入焦点与筛选状态；页面隐藏时暂停；详情切换区间不再整页闪烁 |
| 面板登录 | 用户名与邮箱（大小写不敏感）均可登录；错误密码 401；会话跨刷新与服务重启保持；退出后 Cookie 立即失效 |
| 鉴权边界 | 未登录读写接口 401；会话写请求缺 `X-Requested-With` 返回 403；管理令牌 API 与 `/metrics` 仍可用 |
| 凭据存储 | `admin-user.json` 仅存 PBKDF2-SHA256 哈希（600,000 次迭代），无明文密码；`session.key` 0600 |
| 上报频率 | 30 秒一报，在线/数据延迟/离线状态判定正确 |
| 指标正确性 | CPU/内存/磁盘与 `df`/`free` 交叉核对无偏差 |
| 数据持久化 | state.json 与 gzip history.json 重启后保留，历史快照升级 schema 2 |
| 历史范围 | 1h/6h/24h 原始点；7d/30d/365d 走 1m/5m/1h 聚合，全年合成数据 UI 验证通过 |
| 告警与通知 | 默认规则加载；Outbox 指标正常；插件投递、环境隔离与拒绝全局可写目录在独立实例验证 |
| 主题 | 公开读取默认主题，管理端校验、保存与重启后持久化均正常 |
| Agent 自更新 | 独立实例验证签名安装、启动计数、自动/手动回滚与 7 天冷却 |
| 安全约束 | Agent 依赖表无 `os/exec`、监听端口数 = 0；服务端 `net.Listen` 符号 > 0 |
| 响应式界面 | 1440px 桌面端（侧栏 + 顶栏）与 390px 移动端（底部导航）登录、概览、设置页实测通过 |
| 内存占用 | server 14.3 MB / agent 7.9 MB |

完整操作步骤与升级/回滚方法见 `docs/operations.md`；初次部署的完整测试记录见
`docs/部署与验证报告-20261017.txt`，界面截图见 `docs/screenshots/`。

---

*License: MIT*

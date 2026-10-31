# Rollcall — 轻量机群监控平台

> 曾用名 **Probe**（2026-10-18 改名）。`docs/` 下的两份历史报告仍使用旧名，为当时事实记录，保留原貌。

当前版本 **v1.43.0**（`./rollcall-server -version` 可查）。

> **最短路径（5 步，约 3 分钟）**——细节见下文「五、快速开始」：
>
> ```bash
> # ① 控制端：下载并校验（amd64，其他架构把 amd64 换掉即可；版本号自动取最新 tag）
> V=$(curl -s https://api.github.com/repos/motao123/rollcall/releases/latest | grep -oE '"tag_name": *"v[^"]*"' | cut -d'"' -f4)
> BASE="https://github.com/motao123/rollcall/releases/download/$V"
> curl -fsSLO "$BASE/rollcall-server-linux-amd64" && curl -fsSLO "$BASE/rollcall-agent-linux-amd64"
> curl -fsSLO "$BASE/SHA256SUMS"
> sha256sum -c --ignore-missing SHA256SUMS
> # ② 安装并体验演示模式（-agent-dir 启用「一条命令接入」；正式部署的 systemd 单元见「五」）
> sudo install -d -m 0755 /opt/rollcall/agent
> sudo install -m 0755 rollcall-server-linux-amd64 /opt/rollcall/rollcall-server
> sudo install -m 0755 rollcall-agent-linux-amd64 /opt/rollcall/agent/rollcall-agent-linux-amd64
> /opt/rollcall/rollcall-server -demo -addr 127.0.0.1:19080 -agent-dir /opt/rollcall/agent
> # ③ 浏览器打开 http://127.0.0.1:19080 登录（密码见启动日志，或用 -admin-password 指定）
> # ④ 面板「概览 → ＋ 接入节点」拿到一条安装命令，到目标 Linux 机器上执行
> # ⑤ 回面板确认节点在线、指标在动
> ```
>
> 注意：Agent 目前**仅 Linux 有指标采集**（其他平台可连接但无数据）；对外提供服务请先配好
> HTTPS（见「八、安全设计要点」的三种方式）。

**目录**：[为什么做](#一为什么做这个项目) · [核心功能](#二核心功能) · [数据采集](#三数据采集与展示) ·
[界面设计](#四界面设计) · [快速开始](#五快速开始) · [更新与升级](#六更新与升级) ·
[项目结构](#七项目结构) · [安全设计](#八安全设计要点) · [已知限制](#九已知限制) ·
[容量基线](#十容量基线) · [后续路线](#十一后续路线) · [部署验证](#十二实际部署验证)

一个**零外部依赖**的自托管服务器监控平台。融合 Nezha 的「服务探测能力」与 Komari 的「极简部署与清爽界面」，并按"Agent 只读"的安全原则重新设计。

```
┌────────────────┐                          ┌─────────────────┐
│ rollcall-agent │                          │ rollcall-server │
│  （只读采集）  │      ──▶ 单向 POST       │（Go + 内嵌前端）│
└────────────────┘                          └─────────────────┘
                                                    │
                                            ┌────────────────┐
                                            │ Web UI（原生） │
                                            │    8 个视图    │
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
- **采集粒度**：除根分区与总网卡外，Agent 还会上报每个挂载点（`mounts`）、每块网卡（`nics`）
  与累计磁盘 I/O（`diskReadBytes` / `diskWriteBytes`），节点详情页可展开查看
- **Komari 式轻量实现**：无数据库、单二进制、启动即用
- 每个节点一张卡片：三色资源条 + 迷你 CPU 曲线 + 区域/流量/运行时长
- 节点详情：五张时序图（CPU / 内存 / 磁盘 / 网络速率 / 负载），支持 1h / 6h / 24h / 7d / 30d / 365d 切换；
  长范围自动使用 1m / 5m / 1h 聚合，不会一次返回数十万原始点

### 2. 服务探测（Nezha 强项）
- 协议：HTTP(S) / TCP / **DNS** / **ICMP（ping）**（可配合 `loss`、`latency` 规则）
  · ICMP 回答的是另一个问题：「机器在不在线、往返多久」。服务全挂但机器还在时，
    只有它能区分「网络断了」和「进程挂了」。目前限 IPv4；走**非特权** datagram socket，
    只要控制端内核允许（`net.ipv4.ping_group_range` 覆盖服务用户的组）就不需要 root
    或 `CAP_NET_RAW`，也不给 systemd 单元加任何 capability。内核不允许时，
    探测详情会写明「请确认 sysctl … 覆盖服务用户的组」
- 指标：可用性、延迟、HTTP 状态码、**TLS 证书剩余天数**（不足一天按 1 计，`0`=已过期，`-1`=非 TLS）、
  **抖动（jitter）与丢包率（loss）**（基于该服务最近探测样本计算）
- 24 小时可用率统计 + 最近 2 小时延迟迷你曲线
- **异常服务自动置顶**，一眼看到哪里坏了
- **同目标并排对比**：按主机名聚合同一主机的多条探测（如 HTTPS + TCP + DNS），
  并排显示延迟、丢包/抖动、证书与可用率，便于区分「目标整体异常」与「单条协议/路径失败」。
  各行的「归属节点」只是展示归组——探测统一由控制服务器发起，不代表该节点的本地网络视角
- 后台可对单个目标点「检测」立即执行一次探测，直接显示延迟、HTTP 状态码与证书剩余天数
- 探测由**服务端发起**，不经 Agent —— 单个 Agent 沦陷无法污染其他节点的服务状态
- **默认拒绝内网目标**（回环/RFC1918/链路本地/metadata/CGNAT）：确有内网监控需求时显式加
  `-allow-private-probes`；HTTPS 证书默认严格校验，自签名目标需显式加 `-insecure-probe-tls`
- HTTP 跳转逐跳重新校验地址，3xx 不再默认视为健康；TCP 目标必须是合法的 `host:port`

### 3. 告警与通知（融合三者）
- 规则维度：`offline` / `cpu` / `mem` / `disk` / `swap` / `load` / `load5` / `load15` /
  `tcp_conn` / `procs` / `traffic_quota` / `service_down` / `latency` / `loss` / `jitter` / `cert_days`
- 支持 `gt` / `lt` 比较与 **持续时长抑制**（避免抖动误报）
- 三级严重度：`info` / `warn` / `critical`，可按阈值过滤
- 告警自动恢复（指标回落即置为已恢复）
- **服务类规则可按服务定向**：`serviceIds` 留空表示全部服务；证书临期、服务宕机、丢包、抖动
  都可只对指定探测目标生效
- **流量配额告警**：节点可设置每周期配额（GB）、周期起始日（1–28）与**计费口径**
  （收发合计 / 只计下行 / 只计上行 / 取较大者），跨月自动重置，
  使用率超过默认规则阈值（100%）即告警。口径随商家计费方式设置，切换后当前周期用量立即按新口径重算
- 默认规则开箱即用：失联 3 分钟、CPU>90% 持续 5 分钟、内存>90%、磁盘>85%、
  服务探测失败、证书剩余不足 14 天、流量配额超限
- **规则模板一键套用**：告警页「规则模板」内置上述 7 类预设（带阈值说明），
  可单条创建或「一键补齐缺失」；模板用固定 ID，重复套用不会产生同名规则
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
- **免打扰时段**：可设置起止小时（支持跨午夜）。窗口内只有 `critical` 与恢复通知立即发送，
  其余任务推迟到窗口结束；实际发送时间记录在通知队列中
- **摘要聚合**：`digestMins` 可把普通告警按固定分钟窗口合并为一条摘要，critical 与恢复通知不合并；
  摘要窗口与免打扰同时启用时取较晚的时间
- **自定义模板**：标题/正文支持 `{title}`、`{detail}`、`{node}`、`{rule}`、`{value}`、
  `{severity}`、`{status}`、`{time}`、`{link}` 占位符；所有通道（含 Webhook）统一套用，
  发送测试通知时也会应用当前模板，便于先验证再启用
- **运行报告**：可开启每日/每周/每月报告，在配置的小时（本地时区）汇总节点在线率、
  平均资源、活跃告警、服务异常、周期流量与成本；报告随同现有通知通道发送

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

### 4. 资产台账（两者都没有）
这是 40 台规模最现实的痛点：**忘续费 = 直接丢机器**。

- 字段：服务商、地域、分组、到期日、年付价、币种、备注、国家代码、收藏
- 30 天内到期自动进入「即将到期」列表（7 天内标红）
- 月度成本自动折算（年付 ÷ 12，CNY 按汇率折算为 USD 聚合）
- 节点卡片与台账共用同一份数据，不重复录入
- **到期提醒**：`expiryDays` 控制提前提醒天数（默认 7 天，`-1` 关闭），
  到期当天与提前提醒各发送一次通知，跨重启不会重复轰炸（已发送记录持久化）
- **续费日历**：可登记每次实际续费（日期、金额、币种、备注），
  台账页按月展示续费记录与成本回溯；续费记录随状态快照持久化

### 4.1 跨节点对比与收藏

- 「对比」页按当前实时指标横向比较所有节点：CPU/内存/磁盘、负载、实时网速、
  本周期流量、月摊成本与到期剩余天数
- 支持只看收藏（`favorite=1`），收藏状态在节点/台账中一键切换
- `GET /api/compare` 返回聚合行与 `summary`（收藏数、在线数、在线节点实时下行合计 `trafficIn`（B/s）、全部节点月度成本合计等），
  可直接用于外部报表

### 4.2 加密配置备份

- 设置页可导出**配置级**加密备份（v2 格式）：节点台账、服务、规则、通知配置、
  状态页、主题、续费记录与令牌摘要；不包含历史指标大文件
- 备份文件使用 PBKDF2-SHA256 派生密钥 + AES-256-GCM 加密，导出响应附带
  `filename`、`size` 与 `sha256` 校验值
- 恢复时必须显式提交 `confirm` 与备份密码，并校验摘要；恢复后会保留节点上报令牌，
  Agent 无需重新配置

### 5. 公开状态页（默认开启，可选令牌与逐台隐藏）

> **v1.25.0 起默认开启**（v1.15.2–v1.24.x 为默认关闭）。公开页展示节点名
> （通常等于"服务商 + 地区"）、在线状态与可用率——这是探针的核心卖点，
> 装好即可直接分享 `/status` 链接；**脱敏保证不变**：无 IP、无价格/到期日、
> 无主机指纹。不想公开的实例在「状态页 → 总开关」显式关闭（关闭时对匿名
> 一律 **404**，不是 403——403 等于告诉扫描器这里确实有页面），
> 或配访问令牌把链接变成"知道链接才进得来"的私享页。

#### 5.1 控制台式视图 `/monitor`（v1.41.0）

同一个公开页还提供第二种呈现方式：**`/monitor`** —— 面向"一次看全部"的控制台式布局，
与 `/status` 共用同一份数据和同一道访问门槛（未开放时同样 404）。

| 视图 | 展示内容 |
|---|---|
| 条形图 | 每台一张卡，CPU/内存/磁盘三条进度条（≥60% 转琥珀、≥80% 转红）；第四条「负载」= `load1 ÷ 核数`，便于跨核数比较 |
| 环形图 | 同一组指标画成 3/4 圆环，中心百分比，环下标注 `24/80 GB` 这类紧凑容量对 |
| 列表 | 9 列表格（主机名+分组、CPU、内存、磁盘、负载 1/5/15、入站、出站、运行时长、最后上报），表头可排序 |

配套能力：

- **筛选**：搜索（主机名/地区/系统）+ 状态 + 分组，三种视图共用同一份结果
- **内页详情**：`#/server/<hid>`，8 项资产信息 + 时间范围（24 小时 / 7 天 / 30 天）+ 4 张趋势图
  （CPU / 内存 / 磁盘 / 网络，悬停读取该时刻各序列读数）；负载没有历史，只显示 1/5/15 的当前值
- **实时推送**：订阅 `/api/status/stream`，服务端变更即触发重取（1.2 秒内合并），
  轮询作为兜底而非唯一来源
- **深浅主题**：跟随系统，可手动切换并记住选择（`prefers-color-scheme` 决定首屏，不闪白）
- **最近事件**：直接取自 `/api/status` 的 `activity`，不额外发请求

数据全部来自公开接口（`/api/status` 与 `/api/status/node/{hid}`），**不读管理接口**。
三处刻意的降级：接口未提供的字段（OS、内核、CPU 型号）不显示，不用本地台账去"补"
（会在别人的部署上贴错标签）；**整列都没有值时该列不渲染**，免得列表铺满「—」；
某列缺值时显示「—」而不是 0——"没有数据"和"是 0"是两件事。

界面按成品来做：不出现数据源选择、字段映射表之类的内部约定。数据源由部署位置决定
（同源即真实数据），只在 `file://` 直接打开单文件时使用内置演示数据用于看样式。

> 页面源码在 `monitor-panel/index.html`（单文件，可直接双击预览）。
> 修改后执行 `node monitor-panel/build.mjs` 生成 `internal/web/dist/monitor.{html,css,js}`
> —— 拆成三份是因为 CSP 为 `script-src 'self'`，内联 `<script>` 会被静默拦截。

打开方式（设置页 → 公开页 → 访问门槛，或 API）：

```bash
TOKEN=$(cat /opt/rollcall/data/admin.token)
# 打开，并设一个 16–64 位的访问令牌（不设令牌 = 知道地址即可访问）
curl -sS -X PUT http://127.0.0.1:19080/api/admin/status-page   -H "X-Admin-Token: $TOKEN" -H 'Content-Type: application/json'   -d '{"enabled":true,"token":"改成一个随机串至少16位"}'
# 怀疑链接外泄：一键换链，旧链接立即失效
curl -sS -X POST http://127.0.0.1:19080/api/admin/status-page/token -H "X-Admin-Token: $TOKEN"
```

访问时带 `?t=<令牌>`，首次命中会种一个 HttpOnly Cookie，页面里的静态资源与 SSE
不必再重复带查询串。令牌只是"知道链接才进得来"，不是账号体系。

**逐台隐藏与公开备注**：设置页的「逐台可见性」里取消勾选即从公开页隐藏
（连详情页一起 404）；「公开备注」只出现在公开页，与台账里的内部备注完全分离。

访问 `/status` 得到一个**只读**的状态页，可挂到域名下。
信息架构参考主流探针（Kokoro、ss.akz 等）：**总体健康 → 筛选 → 节点卡片 → 节点详情**，
但数据面严格受后台的「可见性」与「区块」开关约束。

- **总览**
  - 健康环 + 总体结论（全部正常 / 部分异常 / 等待接入）
  - KPI 汇总：在线节点、24h 平均可用率、平均 CPU/内存/磁盘、实时下行、服务健康、活跃告警；
    按容器实际宽度自动排成整行，放不下时等分换行（不会再出现 7+1 的单张孤卡）
  - 搜索（名称/分组/地区/系统）+ 状态筛选 + 分组下拉
  - 节点卡片：状态徽标与数据延迟/离线的"最后上报"提示、CPU/内存/磁盘进度条
    （按 75%/90% 变色）、实时上下行与累计流量、CPU 迷你趋势、**48 段 24h 可用性色块**
  - **国家/地区分布图**与**最近活动流**（告警开启/恢复、节点上下线）可在展示配置中开关；
    只使用显式开启的国家字段，未设置国家的节点不会出现在地图上
  - **事件与最近活动默认只显示 3 / 5 条预览**，卡片右上角的「查看更多」
    进入 `#/events` / `#/activity` 独立页面查看完整列表；列表每页 10 条，
    支持上一页/下一页，避免时间线把首页拉得过长
  - **模块顺序可拖拽调整**：六个区块（节点列表 / 服务探测 / 地区分布 / 可用率 / 事件 / 最近活动）
    都能整体上下拖动，例如把最下方的「可用率」直接拖到最上方
    - 交互：区块右上角悬浮手柄（管理员登录后淡显），拖动松手即生效；
      触屏可拖、键盘可聚焦手柄后 `Alt+↑/↓` 移动一格
    - 布局自适应：区块按新顺序重排，节点列表连同搜索/筛选栏一起移动；
      窄屏（<720px）仍为单列堆叠，顺序不变
    - 持久化：顺序写入服务端配置（`sections`），**刷新、换设备、重启进程都保持**；
      匿名访客看不到手柄，只读浏览按管理员设定的顺序显示
- **刷新节奏**（上报后多久能看到）
  - 面板概览 / 节点列表：**事件驱动**。服务端在数据变化（Agent 上报、节点增删改）时
    通过 `GET /api/stream`（SSE）推送一个版本号，前端收到即刷新；
    实测**上报落地到界面更新 0.2 秒**（此前依赖 5 秒轮询）
  - 局部更新：概览 / 节点列表只替换发生变化的节点行（按 `data-cell` 原位更新），
    KPI、服务商分组抬头、告警条单独刷新；节点增删、筛选结果或告警集合变化时
    才退回整页重绘。滚动位置、搜索词与批量勾选都保留，**勾选中的行也会继续更新**
  - 兜底轮询 30 秒：只处理"时间流逝型"变化（在线 → 掉线没有任何写操作，无法被事件触发）
  - 公开状态页：同样订阅 `GET /api/status/stream`（SSE，只推版本号），
    上报落地后约 0.3 秒刷新；10 秒轮询兜底，切回标签页立即刷新
  - 数据新鲜度的上限由 Agent 上报间隔决定：默认 30 秒（`OnlineTTL/4`，
    可在 Agent 配置中收紧到 5 秒或放宽到 60 秒）。推送只能让已经到达服务端的新数据
    立刻上屏，不能快过采集本身
  - 节点详情：5 秒（图表较重，保持不变）；设置/规则/令牌等表单页：30 秒（避免打断编辑）
  - 所有请求带 12 秒超时：一次挂起的请求不会让轮询永久停摆
  - 为什么用 SSE 而不是 WebSocket：单向推送够用、浏览器原生自动重连、
    穿透反向代理，且服务端用标准库即可实现（保持零外部依赖）
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
| `GET /themes/<name>/status.css\|status.js` | 主题包资源（固定文件名，`Cache-Control: no-cache`） |

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

**两步验证（TOTP）**：设置页可绑定任意标准 TOTP 应用（RFC 6238，6 位/30 秒，±1 窗口）。
启用后登录必须先提交密码、再提交动态码；启用/停用都需要当前密码，停用还需有效动态码。
启用或停用会递增会话版本号，其他设备上的旧会话立即失效。

**范围化 API 令牌**：除固定的 `X-Admin-Token` 外，可在设置页创建 `rct_` 前缀的自动化令牌：

| scope | 权限 |
|---|---|
| `read` | 全部只读接口（`/api/overview`、`/api/nodes`、`/api/services`、`/api/alerts`、`/api/compare` 等） |
| `admin` | 全部接口与写操作 |

- 明文只在创建响应中出现一次；服务端仅保存 SHA-256 摘要，列表与日志永不回显
- 可设置有效期（最短 30 分钟，按分钟或天；留空/0 表示永不过期），到期自动拒绝；最近使用时间会持久化，重启后令牌仍然有效
- 可限定路径前缀（如 `/api/overview`、`/api/nodes/n1`），把脚本/AI 令牌收窄到最少接口；
  匹配按路径段边界进行，`/api/nodes/n1` 不会放行 `/api/nodes/n10`，留空表示全部
- 令牌通过 `Authorization: Bearer <token>` 使用；`read` 令牌访问管理写接口返回 401

### 6. 外观主题（受限沙箱）

- 设置页可配置明暗模式、浅色/深色强调色、圆角（0–24px）与紧凑密度
- 服务端只接受固定字段（`mode` / `accent` / `accentDark` / `radius` / `compact`），
  未知字段与非法颜色直接拒绝；**永远不接受 CSS 文本**
- 前端只把这些值映射到白名单 CSS 变量；公开状态页同步应用，且不新增任何数据暴露
- 主题随设置持久化；`GET /api/theme` 公开下发（仅样式），
  管理端用 `GET/PUT /api/admin/theme` 读写

#### 公开页主题包（目录分发）

- 在 `<数据目录>/themes/<name>/` 放一个 `theme.json`，可选 `status.css` 与 `status.js`（至少一个）：
  ```json
  {"name":"aurora","title":"极光","version":"1.0","author":"ops","description":"深色玻璃"}
  ```
  目录名只能是小写字母、数字与连字符（1–32 位）；`name` 若填写必须与目录名一致；
  清单未知字段、符号链接、超过 1 MiB 的资源都会在扫描时被拒绝
- 设置页「外观主题 → 公开状态页主题包」选择后写入配置；公开页在
  `<html data-theme-pack="…">` 上标记主题，并在内置样式/脚本之后加载主题资源
- `GET /themes/<name>/status.css|status.js` 公开下发，`Cache-Control: no-cache`；
  只允许这两个固定文件名，主题名与文件名都做白名单校验
- 主题包缺失或校验失败时公开页**静默回退**为内置外观，管理端列表会显示原因与警示
- **安全边界**：主题脚本与站点同源运行（CSP 仍为 `script-src 'self'`），
  能读取公开页可见的数据并发起同源请求；安装主题等同授予管理员权限，只使用可信来源。
  主题资源不在加密备份内，迁移时需单独备份 `themes/` 目录

### 7. Agent 签名自更新（可选，默认关闭）

- Agent 从配置中的**清单地址主动拉取**更新，不开放入站端口，服务端不参与、也无法下发更新指令
- 清单是 Ed25519 签名信封，包含版本、平台资产 URL、SHA-256 与大小；签名、哈希或大小不符即拒绝
- 更新包必须是小端 ELF 且架构匹配，下载到同目录后原子替换，保留 `.prev` 备份
- 新版本连续 3 次启动仍未完成一次成功上报时自动回滚；失败版本 7 天内不再自动尝试
- 手动回滚 `rollcall-agent -rollback`；立即检查 `rollcall-agent -update-now`
- 自更新需要 Agent 对其二进制所在目录有写权限，systemd 加固下还需放行该目录
  （见运维手册）；默认安装路径 `/usr/local/bin` 归 root 所有，因此默认不会自动更新

---

## 三、数据采集与展示

### 采集方式：只读、单向、无指令通道

Agent 默认每 30 秒读取以下文件（`interval` 可调，最大 60 秒），**不做任何其他操作**：

| 指标 | 来源 |
|---|---|
| CPU 使用率 | `/proc/stat` 两次差值 |
| 内存 / Swap | `/proc/meminfo`（用 `MemAvailable` 算真实可用） |
| 负载 / 进程数 | `/proc/loadavg` |
| 磁盘 | `statfs`：根分区 + `/proc/mounts` 中每个真实挂载点 |
| 磁盘 I/O | `/proc/diskstats`（累计读写字节，排除分区重复计数） |
| 网络收发 | `/proc/net/dev`（汇总 + 每块物理网卡；排除 lo / veth / docker） |
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
- 快照带 `schemaVersion`（当前为 4）：更高版本的快照会被隔离为 `state.json.newer-<时间>` 并拒绝载入，
  避免新结构被旧程序静默读成零值；v2 新增通知队列、投递统计与静默期，
  v3 将历史聚合写入独立文件，v4 新增续费记录、API 令牌摘要、活动事件、
  到期提醒发送记录与流量周期；
  旧版 v1/v2/v3/无版本快照仍可直接载入
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

八个视图：**概览 / 节点 / 对比 / 服务 / 告警 / 台账 / 状态页 / 设置**，左侧导航切换，节点详情走 `#/nodes/{id}`。
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
- 构建机：**Go 1.27+**（`go.mod` 声明 `go 1.27.3`；低版本会触发 `GOTOOLCHAIN` 自动下载工具链，
  离线构建机会直接失败）、git、curl、make（构建分发产物时需要）
- 可选：gcc（仅 `go test -race` 需要；普通构建与交叉编译不需要）
- 生产端口默认只监听回环，对外请用反向代理或内置 TLS
- 规划基线（40 台节点、30 秒上报）：**1 vCPU / 1.5–2GB 内存 / 5GB 磁盘**；
  10 台以内 512MB 起步。服务端内存随原始环跑满而增长（实测 40 台 RSS 约 361MB），
  快照期间还有峰值，不能按空环的几十兆估算；Agent 只读 `/proc`，占用可忽略。
  完整实测数据与估算方法见「[十、容量基线](#十容量基线)」。

> Ubuntu 22.04 的 `apt install golang-go` 目前是 Go 1.18，**无法构建本项目**。
> 请使用官方二进制包或更新的发行版仓库；构建机为 arm64 时把 `linux-amd64` 换成 `linux-arm64`：
>
> ```bash
> GO_VERSION=1.27.3
> curl -fsSLO "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz"
> sudo rm -rf /usr/local/go
> sudo tar -C /usr/local -xzf "go${GO_VERSION}.linux-amd64.tar.gz"
>
> # /usr/local/go/bin 默认不在非交互 shell 的 PATH 里：export 只对当前会话有效，
> # 下次 ssh 进去（或 systemctl 拉起的构建任务）又会变成 "go: command not found"。
> echo 'export PATH=$PATH:/usr/local/go/bin' | sudo tee /etc/profile.d/go.sh
> export PATH=$PATH:/usr/local/go/bin
> go version   # 确认 >= 1.24
> ```
>
> 不想装 Go 也可以直接用发布包里的预编译二进制，见下一节的「方式 0」。
> 容器环境可用仓库根目录的 `Dockerfile` 自行构建镜像
> （`docker build --build-arg VERSION=v1.20.0 -t rollcall .`，
> distroless 运行时、nonroot、数据卷 `/data`，详见文件内注释）。

### 1. 获取源码并构建

**方式 0：不装 Go，直接用发布包**（推荐，`v1.10.5` 起 CI 会随 tag 发布预编译二进制）

```bash
VERSION=$(curl -s https://api.github.com/repos/motao123/rollcall/releases/latest | grep -oE '"tag_name": *"v[^"]*"' | cut -d'"' -f4)
# ↑ 自动取最新 tag；要装特定版本就手动指定，如 VERSION=v1.26.0
ARCH=amd64          # amd64 / arm64 / arm / 386
BASE="https://github.com/motao123/rollcall/releases/download/${VERSION}"

curl -fsSLO "$BASE/rollcall-server-linux-$ARCH"
curl -fsSLO "$BASE/rollcall-agent-linux-$ARCH"
curl -fsSLO "$BASE/SHA256SUMS"
sha256sum -c --ignore-missing SHA256SUMS      # 必须先校验再用
chmod +x "rollcall-server-linux-$ARCH" "rollcall-agent-linux-$ARCH"
# ↑ GitHub 资产没有执行位：想先 ./rollcall-server-linux-$ARCH -version 确认就得先 chmod

sudo install -d -m 0755 /opt/rollcall
sudo install -m 0755 "rollcall-server-linux-$ARCH" /opt/rollcall/rollcall-server
sudo install -m 0755 "rollcall-agent-linux-$ARCH" /usr/local/bin/rollcall-agent
/opt/rollcall/rollcall-server -version        # 应输出 $VERSION
```

发布包由 CI 用 `make linux` 构建（`CGO_ENABLED=0 -trimpath -buildvcs=false`，静态链接，无 glibc 版本要求），
`SHA256SUMS` 覆盖全部 16 个产物（server / agent / mcp / rollsign × 4 架构，`v1.21.0` 起）。
`v1.10.5` 之前的 tag 没有发布资产，只能走下面的源码构建。

**方式 1：从源码构建**

```bash
git clone https://github.com/motao123/rollcall.git
cd rollcall
go build -o rollcall-server ./cmd/server
go build -o rollcall-agent ./cmd/agent

# 交叉编译示例
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o rollcall-server ./cmd/server
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o rollcall-agent ./cmd/agent
```

> 项目零外部依赖，`go build` 不需要联网。`make linux` 会一次构建
> server/agent/rollsign 并生成 amd64/arm64/arm/386 的分发产物，同时写出
> `dist/SHA256SUMS`；只做单机构建时可以不装 make。需要生成自更新签名清单时，
> 在**签名机**上执行 `make rollsign` 构建 `./rollsign`（不要复制到被监控节点）。
>
> 构建参数包含 `-trimpath -buildvcs=false`：同一份源码配合相同的 Go 版本，
> 在任意目录（包括 git 工作区与非 git 目录）都能得到字节一致的二进制。
> 发布时先用 `cd dist && sha256sum -c SHA256SUMS` 自检，再把校验和与版本号一起记录。
>
> 版本号通过 ldflags 注入：`make linux VERSION=v1.14.0`。直接 `go build` 时
> 版本为 `dev`；`./rollcall-server -version` 与 `/metrics` 的 `rollcall_build_info` 均可查看。
>
> 并发回归测试用 `go test -race ./...`（需要 gcc/CGO）；CI 已单独配置 `race` job。
>
> 在 Windows 上跑 `go test ./...` 会**跳过**几个依赖 POSIX 权限位的用例
> （插件可执行位、`0600` 文件权限、原子替换回滚），这是平台差异而非失败；
> 对应的安全断言在 Linux/CI 上照常执行。

### 2. 预览 UI（可选）

```bash
# 演示模式同样需要登录；显式给一个临时密码，避免去临时目录翻初始密码
ROLLCALL_ADMIN_PASSWORD='demo-password-1234' \
  ./rollcall-server -addr 127.0.0.1:9188 -demo
```

打开 `http://127.0.0.1:9188`，用 `admin` / 上面设置的密码登录。
演示模式使用临时数据目录，退出后自动删除，不会污染生产 `state.json`。
未设置密码时，服务端会随机生成并写入该临时目录的 `initial-admin-password.txt`，
启动日志会打印完整路径。

### 3. 安装服务端（systemd）

> **三种部署形态**（本小节是 systemd 主线；另两种见 `deploy/`）：
>
> | 形态 | 文件 | 适用 |
> |---|---|---|
> | systemd 二进制 | `deploy/rollcall-server.service` + `deploy/rollcall.env.example` | 单机生产，推荐（单元已含 `ProtectSystem` 等加固） |
> | Docker Compose | `deploy/docker-compose.yml` | 已有容器编排；含命名卷、时区、`-trusted-proxies` 预设 |
> | 反向代理 | `deploy/nginx.conf` | TLS 终结。★ 必须 `proxy_buffering off`（SSE 否则会卡顿），并给服务端配 `-trusted-proxies` |
>
> 三种形态的共同约束：**面板不支持子路径部署**（前端使用绝对路径），只能挂在域名根。

```bash
sudo useradd --system --home /opt/rollcall --shell /usr/sbin/nologin rollcall
sudo install -d -o rollcall -g rollcall -m 0750 /opt/rollcall /opt/rollcall/data
sudo install -m 0755 rollcall-server /opt/rollcall/rollcall-server
sudo install -m 0644 deploy/rollcall-server.service /etc/systemd/system/
sudo systemctl daemon-reload
```

**先不要启动服务**。下一步需要选择管理员初始化方式；如果服务已经启动过，
方式 A 不会再生效，请按方式 B 读取初始密码，或使用下方「重置管理员密码」流程设置密码。

`deploy/rollcall-server.service` 默认**不开启**私网探测/通知。确需监控本机服务或向内网
webhook 推送时，用 systemd drop-in 追加 `-allow-private-probes` / `-allow-private-notify`
（写法见 [`docs/operations.md`](docs/operations.md)），不要直接修改模板默认值。

> 日常运维（备份恢复、升级回滚、令牌轮换、故障排查）见 [`docs/operations.md`](docs/operations.md)。

> 服务端收到 SIGINT/SIGTERM 后会停止接收新请求、等待在途请求结束并立即保存快照，
> systemd 重启不会丢失最近一次自动保存（60 秒）之后的数据。

### 4. 首次登录

首次启动时会在 `data/admin-user.json` 创建唯一管理员账号。两种初始化方式任选，
**方式 A 必须在第一次启动之前完成**：

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
```

方式 B 不需要配置文件，直接启动后读取服务端生成的密码：

```bash
# 启动服务（两种方式都在这里首次启动）
sudo systemctl enable --now rollcall-server
curl -fsS http://127.0.0.1:19080/readyz   # 200 表示存储与数据目录均正常

# 方式 B：密码只写入 data/initial-admin-password.txt（0600），日志只记录路径
sudo cat /opt/rollcall/data/initial-admin-password.txt
```

打开 `http://127.0.0.1:19080`（或反代域名），输入用户名/邮箱与密码登录。
登录后可在「设置 → 管理员账号」直接编辑用户名/邮箱、修改密码或退出登录。
保存前必须输入当前密码；修改成功后当前浏览器自动续签，其他设备上的旧会话立即失效。
首次登录成功后，服务端会自动删除 `initial-admin-password.txt`。

> 环境变量只用于**首次创建**账号。建议登录后立即按下方重置流程换成自己的强密码，
> 然后删除 `/etc/rollcall/server.env` 中的密码行（单元使用 `EnvironmentFile=-`，
> 文件缺失不影响启动），避免明文密码长期留在磁盘。

> 密码只在创建或显式重置时写入。正常重启不会用环境变量覆盖已保存的密码。

对外访问请用 SSH 隧道或反向代理，不要直接把 19080 暴露到公网。

#### 重置管理员密码（忘记密码时）

`-reset-admin-password` 是**服务端二进制的启动参数**，不是 shell 命令，不能直接输入执行。
忘记密码时，先停止服务，再以 `rollcall` 用户执行一次重置（命令完成后立即退出），最后启动服务：

```bash
read -rsp '新密码: ' PW && echo      # 输入不回显，也不进入 shell history
sudo systemctl stop rollcall-server
sudo runuser -u rollcall -- env ROLLCALL_ADMIN_PASSWORD="$PW" \
  /opt/rollcall/rollcall-server -data /opt/rollcall/data -reset-admin-password
unset PW
sudo systemctl start rollcall-server
```

- 新密码至少 12 位；重置会更新会话版本，旧 Cookie 全部失效，需用新密码重新登录。
- 必须以 `rollcall` 用户执行，否则 `admin-user.json` 的属主会变成 root，服务将无法写入。
- 已登录时无需停服，可在「设置 → 管理员账号」直接修改密码。

更完整的运维说明见 [`docs/operations.md`](docs/operations.md)。

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
# 控制端：准备分发目录（用 make linux 的产物，四种架构一并放入）
sudo install -d -o rollcall -g rollcall -m 0755 /opt/rollcall/agent
sudo install -m 0755 dist/rollcall-agent-linux-{amd64,arm64,arm,386} /opt/rollcall/agent/
# 在 rollcall-server.service 的 ExecStart 追加 -agent-dir /opt/rollcall/agent
# （drop-in 写法见 docs/operations.md；改完 systemctl daemon-reload && systemctl restart rollcall-server）
```

```bash
# 目标节点：一条命令（必须以 root 运行）
curl -fsSL https://你的面板/agent/install.sh | sudo sh -s -- https://你的面板 <node-id> <token>

# 后续升级：只替换二进制，保留配置与 unit
curl -fsSL https://你的面板/agent/install.sh | sudo sh -s -- --upgrade
```

脚本会自动识别架构（amd64/arm64/arm/386）、创建 `rollcall-agent` 低权限用户
（与 `deploy/rollcall-agent.service` 姿态一致）、写入 0640 配置、生成 systemd 单元；
没有 systemd 的容器里以同一低权限用户后台常驻，日志写入 `/var/log/rollcall-agent.log`。
升级模式只替换二进制，保留配置、unit 与原属主；没有 systemd 时会重启后台进程，
启动失败自动回滚 `.prev`。完整行为见「六、更新与升级」。
未配置 `-agent-dir` 时 `/agent/*` 返回 404，仍可按上面的手动步骤安装。

脚本下载二进制后会向 `/agent/download.sha256?arch=<arch>` 查询服务端计算的 SHA-256，
校验一致才替换（截断文件、代理错误页都会中止）；停止旧进程时按 `/proc/<pid>/exe`
精确匹配目标二进制，不会误杀命令行里含有相同字符串的其他进程。

> 脚本本身不含任何密钥（node id / token 由命令行参数传入），因此可以公开下载与审计。
> 但它以 `curl | sh` 方式执行，**只在 HTTPS 或 SSH 隧道下使用**：明文 HTTP 下中间人
> 可以替换脚本或二进制。SHA-256 只能防止传输损坏，不是密码学签名——它同样来自
> 控制端，无法抵御能篡改 HTTPS 响应的攻击者。

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

反代最小配置（可直接粘贴）。⚠ 面板的实时刷新走 **SSE**（`/api/stream`），
nginx 默认缓冲会把它攒住不吐，表现为「实时」角标停滞——两份配置都已是关闭缓冲的写法；
同时记得给服务端配 `-trusted-proxies`，否则日志与失败限速会把所有访客算成同一个 IP：

```caddy
# Caddyfile —— 自动签发/续期证书，最省事（my.rollcall.example.com 换成你的域名）
my.rollcall.example.com {
    reverse_proxy 127.0.0.1:19080
    # Caddy 对 SSE 默认流式透传，无需额外配置
}
```

```nginx
# /etc/nginx/sites-available/rollcall（证书用 certbot 签）
server {
    listen 443 ssl;
    server_name my.rollcall.example.com;
    ssl_certificate     /etc/letsencrypt/live/my.rollcall.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/my.rollcall.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:19080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;   # 服务端据此生成正确的安装命令
        proxy_buffering off;                          # ★ SSE 必需：否则实时刷新停滞
        proxy_read_timeout 1h;                        # SSE 长连接别被默认 60s 砍断
    }
}
```

反代起来之后还有两步，否则会有「能打开但通知里的链接点不开」这类问题：

1. **面板里填对外地址**：设置页 →「⚙ 面板对外地址（域名反代）」填
   `https://my.rollcall.example.com`。它决定**告警通知里的链接**与
   **Agent 接入命令里的地址**（原来只能用 `-panel-link` 启动参数，v1.33.0 起可在面板里改并落盘；
   启动参数仍然优先，方便用环境变量写死）。
2. **服务端加 `-trusted-proxies 127.0.0.1/32`**（代理与面板同机时）。不配的话
   记不到真实客户端 IP、登录失败限速会把所有访客算成同一个 IP，登录 Cookie 也不会带 `Secure`。

**不支持子路径部署**（`https://example.com/rollcall/`）：前端路由与 API 都是绝对路径。
请用独立域名或子域名，根路径反代到 `127.0.0.1:19080` —— 面板会直接拒绝带路径的地址。

完整参数：

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-addr` | `127.0.0.1:19080` | 监听地址；非回环需满足上面的安全条件 |
| `-demo` | false | 载入 15 台演示数据，使用临时数据目录，不发起真实探测 |
| `-panel-link` | 空 | 面板对外地址（告警链接与接入命令）。优先于面板里的设置；留空则用面板设置，再留空则用当前访问地址 |
| `-timezone` | 空 | 时区（IANA 名，如 `Asia/Shanghai`）。留空跟随系统 TZ；**服务器（尤其容器）默认 UTC 时，日报/免打扰/流量周期都会按 UTC 算**，设置页会显示当前生效时区 |
| `-tls-cert` / `-tls-key` | 空 | 同时提供则启用 HTTPS |
| `-auth` | 空 | 兼容用 `user:password` Basic Auth（面板 UI 不使用；保留给旧集成） |
| `-allow-public` | false | 已废弃，仅为兼容旧启动参数保留 |
| `-allow-insecure` | false | 允许非回环地址使用明文 HTTP |
| `-allow-private-probes` | false | 允许 HTTP/TCP 探测内网与回环目标 |
| `-allow-private-notify` | false | 允许通知通道访问内网/回环目标（自建 Gotify/Bark/Webhook） |
| `-notify-plugins` | 空 | 通知插件目录；显式指定后加载顶层 `*.json` 清单 |
| `-agent-dir` | 空 | Agent 二进制分发目录（内含 `rollcall-agent-linux-{amd64,arm64,arm,386}`），启用面板里的「一条命令接入」与 `/agent/download.sha256`；未配置时 `/agent/*` 返回 404 |
| `-trusted-proxies` | 空 | 可信反向代理 CIDR（逗号分隔，如 `127.0.0.1/32,10.0.0.0/8`）；留空只信任回环。放在反代后面时必须配置，否则日志与限速会把所有访客当成同一个 IP |
| `-insecure-probe-tls` | false | 探测 HTTPS 时跳过证书校验（自签名目标） |
| `-probe-interval` | `10s` | 探测调度粒度；每个服务按自身 `interval` 执行 |
| `-save-interval` | `60s` | 自动保存快照间隔（10s–1h）；退出时还会强制保存一次 |
| `-backup-dir` | `<data>/backups` | 自动备份目录；仅在配置备份密码后启用 |
| `-backup-interval` | `24h` | 自动备份间隔（1m–30d） |
| `-backup-keep` | `14` | 自动备份保留份数（1–365） |
| `-backup-password-file` | 空 / `ROLLCALL_BACKUP_PASSWORD` | 自动备份加密密码（文件需 0600，内容至少 8 字符）；留空则关闭自动备份 |
| `-history-1m-retention` | `168h` | 1 分钟聚合保留期（≥1m、为 60s 整数倍） |
| `-history-5m-retention` | `720h` | 5 分钟聚合保留期（≥5m、为 300s 整数倍） |
| `-history-1h-retention` | `8760h` | 1 小时聚合保留期（≥1h、为 3600s 整数倍） |
| `-cny-rate` | `7.2` | CNY 折算 USD 的汇率（仅用于成本聚合展示） |
| `-admin-user` | `admin` / `ROLLCALL_ADMIN_USER` | 面板管理员用户名（2–32 位字母/数字/点/下划线/连字符） |
| `-admin-email` | 空 / `ROLLCALL_ADMIN_EMAIL` | 可选邮箱，设置后可用邮箱登录 |
| `-admin-password` | 空 / `ROLLCALL_ADMIN_PASSWORD` | 初始密码（至少 12 位）；仅在首次创建或重置时生效，留空则随机生成并写入 `data/initial-admin-password.txt`（0600，首次登录后自动删除） |
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
| PUT | `/api/admin/nodes/{id}` | 编辑台账字段（保留 ID 与上报令牌）。**整表替换**：未提交的字段会回到零值，脚本改单个字段也要带全台账（含 `trafficMode`） |
| POST | `/api/admin/nodes/{id}/rotate-token` | 轮换上报令牌（旧令牌立即失效） |
| DELETE | `/api/admin/nodes/{id}` | 删除节点 |
| GET/POST | `/api/admin/services` | 列出 / 创建探测目标（`enabled` 默认 true） |
| PUT | `/api/admin/services/{id}` | 编辑探测目标；省略 `enabled` 时保留原状态 |
| POST | `/api/admin/services/{id}/test` | 立即执行一次探测并返回状态码/延迟/证书天数（不写历史） |
| DELETE | `/api/admin/services/{id}` | 删除探测目标 |
| GET/POST | `/api/admin/settings` | 读取（脱敏）/ 保存通知配置 |
| GET | `/api/admin/health` | 实例自身状态：内存/协程/上报速率/快照分段耗时/通知队列 |
| GET | `/api/live` | 全部节点的「当前态」轻量视图（CPU/内存/磁盘/速率/状态，不含资产字段与 sparkline） |
| GET | `/api/stream` | SSE 事件流（需管理会话或令牌）：数据变化时推 `change`，25 秒心跳 |
| GET/PUT | `/api/admin/theme` | 读取 / 保存外观主题（固定字段，未知字段拒绝） |
| GET/PUT | `/api/admin/theme-packs` | 扫描 / 选择公开页主题包（`{"name":"aurora"}`，空串恢复内置；无效包当场 400） |
| POST | `/api/admin/test-notify` | 发送测试通知，可带 `{"channelId":"..."}` 只测单个 |
| GET/POST | `/api/admin/rules` | 列出 / 创建告警规则 |
| GET/PUT/DELETE | `/api/admin/rules/{id}` | 查看 / 修改 / 删除单条规则 |
| GET | `/api/admin/rule-meta` | 规则表单元数据（指标/级别/时长选项 + 内置规则模板及其「已应用」状态，需令牌） |
| GET | `/api/admin/silences` | 当前生效的通知静默 |
| PUT/DELETE | `/api/admin/silences/{global\|ruleId}` | 设置（`{"minutes":60,"reason":"..."}`）/ 取消静默 |
| GET | `/api/admin/notify/jobs` | 通知队列最近 100 条任务与累计统计 |
| POST | `/api/admin/notify/jobs/{id}/retry` | 手动重试失败/被拦截任务（重置尝试次数） |
| GET/PUT | `/api/admin/status-page` | 读取 / 保存公开页配置（标题、公告、展示天数、字段可见性、区块开关、区块顺序 `sections`） |
| GET/POST | `/api/admin/maintenance` | 列出 / 新建维护窗口（`{title,reason,nodeIds,startAt,endAt}`，省略时间为「现在起 60 分钟」） |
| DELETE | `/api/admin/maintenance/{id}` | 删除维护窗口；窗口内该节点的告警自动抑制并在窗口结束后恢复评估 |
| GET/POST | `/api/admin/incidents` | 列出 / 新建人工事件（`{title,body,level,nodeIds,startAt,endAt}`） |
| DELETE | `/api/admin/incidents/{id}` | 删除人工事件 |
| POST | `/api/admin/nodes/batch` | 批量改台账：`{ids,action,value}`，`action` ∈ `group\|provider\|region\|expire\|currency\|delete`。任一行 ID 不存在则整批拒绝，避免改一半 |

公开接口（无需任何凭据）：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/status` | 公开状态页 HTML |
| GET | `/monitor` | 控制台式视图（条形图 / 环形图 / 列表 + 内页详情），与 `/status` 同门槛 |
| GET | `/api/status` | 公开状态页数据（严格脱敏，节点只带 `hid` 句柄） |
| GET | `/api/status/node/<hid>` | 单节点详情（指标/曲线/逐日可用率），受展示配置裁剪；区块关闭或句柄无效均 404 |
| GET | `/api/status/incidents` | 事件时间线（告警自动事件 + 人工登记事件合并后按时间倒序） |
| GET | `/api/status/stream` | 公开 SSE：数据变化时只推版本号（不含节点数据），供状态页实时刷新 |
| GET | `/api/theme` | 公开外观主题（仅固定样式字段） |
| GET | `/status.js` | 公开状态页脚本 |
| GET | `/monitor.css`、`/monitor.js` | 控制台式视图的样式与脚本（CSP `script-src 'self'` 要求外链） |
| GET | `/healthz` | 存活检查（进程能响应即返回 `ok`） |
| GET | `/readyz` | 就绪检查：存储已载入、数据目录可写、快照未持续保存失败；升级脚本据此决定是否回滚 |

管理接口与 CSV 导出接受两种凭据：浏览器会话 Cookie（写操作需带
`X-Requested-With: XMLHttpRequest`）或请求头 `X-Admin-Token`（供自动化使用）。
管理令牌首次启动时自动生成到 `data/admin.token`（0600），也可用 `-admin-token` 显式指定。
`/api/` 下的 JSON 与 CSV 响应在客户端支持时自动 gzip 压缩：365 天曲线从约 2.5MB 降到约 300KB，慢速链路下也能正常打开。

所有路由的鉴权级别集中声明在服务端路由表中（`public` / `login` / `node-token` /
`readonly` / `admin` / `metrics`）；新增路由若漏填级别，服务端会在启动时直接失败，
不再依赖易漏改的路径白名单。`/status.js`、`/monitor.js`、`/monitor.css` 单独登记为公开，因此公开页面始终可正常加载。

账号登录端点：

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/auth/login` | `{"identifier":"用户名或邮箱","password":"..."}`；成功写入 HttpOnly 会话 Cookie |
| GET | `/api/auth/session` | 返回 `{authenticated,user?,expiresAt?}`；未登录也是 200 |
| POST | `/api/auth/logout` | 清除会话 Cookie（幂等） |
| POST | `/api/admin/account` | `{username,email,currentPassword}`；修改资料并续签当前会话 |
| POST | `/api/admin/account/password` | `{currentPassword,newPassword}`；修改密码并续签当前会话 |

仪表盘只读接口（包含主机名、内核、服务商、价格、到期日等资产信息，不再匿名开放）：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/overview` | KPI 汇总 |
| GET | `/api/nodes` | 节点列表与最新指标 |
| GET | `/api/nodes/{id}` | 节点详情 |
| GET | `/api/nodes/{id}/series?range=1h\|6h\|24h\|7d\|30d\|365d` | 历史时序（长范围自动降采样） |
| GET | `/api/services` | 服务探测状态 |
| GET | `/api/services/compare` | 同主机多探测并排数据（含采集视角说明） |
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

> Cookie 不区分端口：在同一浏览器里再登录 `127.0.0.1` 其他端口上的测试实例，
> 会覆盖生产面板的登录态。测试完重新登录生产面板即可。

**B. 面板反向代理（推荐长期使用）**

*宝塔*：网站 → 添加站点 → 站点设置 → 反向代理 → 目标 `http://127.0.0.1:19080`，SSL 可一键申请。

*1Panel*：网站 → 创建网站 → 反向代理类型 → 代理地址填 `http://127.0.0.1:19080`。

*Nginx（等价手写配置）*：

```nginx
server {
    listen 443 ssl;
    server_name monitor.example.com;

    ssl_certificate     /etc/letsencrypt/live/monitor.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/monitor.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:19080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

*Caddy*：

```caddyfile
monitor.example.com {
    reverse_proxy 127.0.0.1:19080
}
```

反代与面板同机时，服务端要加 `-trusted-proxies 127.0.0.1/32`；反代在另一台机器时
填其真实地址/网段。否则会话 Cookie 的 `Secure` 判断、审计日志与登录限速都会把
所有访客当成同一个来源 IP。

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

## 六、更新与升级

控制端、Agent 与安装脚本是三条独立的更新线。先确认当前版本，再按对象操作：

| 对象 | 更新方式 | 需要保留的东西 |
|---|---|---|
| 控制端 `rollcall-server` | 替换二进制并重启 | `data/` 目录（使用前先备份） |
| 被控端 `rollcall-agent` | `install.sh --upgrade`（推荐）或手动替换 | `/etc/rollcall/agent.json`、systemd 单元 |
| `/agent/install.sh` | 随控制端二进制自动更新 | —（由运行中的服务端实时生成） |
| `-agent-dir` 分发的 Agent | 每个版本替换分发目录里的文件 | —（仅影响后续安装/升级拉取） |

> 服务端**不支持**远程自更新，也不会向 Agent 下发更新指令：更新动作始终由运维在机器上执行。
> Agent 的可选签名自更新是另一条独立路径（由本机配置的清单触发），见「二、核心功能」第 7 节。

### 1. 控制端

面板、API、公开状态页和 `/agent/install.sh` 都编译在同一个 `rollcall-server` 二进制里，
因此升级控制端只需要换二进制：

```bash
# 方式 A：自己构建（注意产物名带 -linux-<arch> 后缀，dist/rollcall-server 并不存在）
make linux VERSION=v1.14.0
(cd dist && sha256sum -c SHA256SUMS)
scp dist/rollcall-server-linux-amd64 root@<控制端>:/tmp/rollcall-server.new
```

```bash
# 方式 B：直接用发布包（v1.10.5 起 CI 随 tag 发布，无需构建机）
curl -fsSLO "https://github.com/motao123/rollcall/releases/download/v1.14.0/rollcall-server-linux-amd64"
curl -fsSLO "https://github.com/motao123/rollcall/releases/download/v1.14.0/SHA256SUMS"
sha256sum -c --ignore-missing SHA256SUMS
mv rollcall-server-linux-amd64 /tmp/rollcall-server.new
```

```bash
# 控制端（先备份；完整备份/回滚说明见 docs/operations.md）
sudo /opt/rollcall/rollcall-server -version        # 记录旧版本
sudo systemctl stop rollcall-server
sudo cp -a /opt/rollcall/data /var/backups/rollcall/data-$(date +%F-%H%M)
sudo cp -p /opt/rollcall/rollcall-server /opt/rollcall/rollcall-server.prev
sudo install -m 0755 /tmp/rollcall-server.new /opt/rollcall/rollcall-server
sudo systemctl start rollcall-server
curl -fsS http://127.0.0.1:19080/readyz
```

回滚：

```bash
sudo systemctl stop rollcall-server
sudo install -m 0755 /opt/rollcall/rollcall-server.prev /opt/rollcall/rollcall-server
# 若新版本已写入更高 schema 的快照，还需恢复上面备份的 data 目录
sudo systemctl start rollcall-server
```

unit 文件只有在该版本明确要求时才需要同步替换；替换后执行 `systemctl daemon-reload`。

**一键升级/回滚脚本**（可选）：`deploy/rollcall-server-upgrade.sh` 把上面的步骤收敛为一条命令，
适合脚本化发布。**先配置发行签名（推荐，首次升级前完成）**：

```bash
# 签名机（不要放在被监控节点）：生成独立的发行密钥并签名发布产物
make rollsign
./rollsign keygen -out /root/rollcall-release.key
./rollsign pubkey -key /root/rollcall-release.key -out /root/rollcall-release.pub
make linux VERSION=v1.14.0
make sign-release ROLLSIGN_KEY=/root/rollcall-release.key VERSION=v1.14.0
# dist/rollcall-server-linux-<arch>.sig 与二进制一起分发

# 控制端（一次性）：安装公钥与验签工具
sudo install -d -m 0755 /etc/rollcall
sudo install -m 0644 rollcall-release.pub /etc/rollcall/release.pub
sudo install -m 0755 rollsign-linux-amd64 /opt/rollcall/rollsign
```

```bash
# 在控制端本机执行；--sha256 仍为必填，用于快速发现传输损坏/拉错文件。
# 注意：--sha256 要填「发布方给出的」校验和（dist/SHA256SUMS 或 Release 页面），
# 不要在目标机上现算刚下载的那个文件——那样两边永远相等，等于没校验。
sudo sh deploy/rollcall-server-upgrade.sh upgrade \
  --file /tmp/rollcall-server.new \
  --signature /tmp/rollcall-server.new.sig \
  --sha256 <发布记录里的 64 位 hex> \
  --service rollcall-server \
  --health http://127.0.0.1:19080/readyz

# 回滚到升级前的 .prev（恢复本机文件，不需要签名）
sudo sh deploy/rollcall-server-upgrade.sh rollback --service rollcall-server \
  --health http://127.0.0.1:19080/readyz
```

- **`/etc/rollcall/release.pub` 存在时必须验签**：签名覆盖版本号、SHA-256 与文件大小，
  验签在备份/替换之前完成；签名版本还要与二进制 `-version` 及升级后的 `/readyz` 双重核对。
  公钥只从本机路径读取，下载源被篡改时不会装上未签名或版本不符的二进制
- `--pubkey` / `--rollsign` 可覆盖默认路径（公钥默认 `/etc/rollcall/release.pub`，
  验签工具优先 `/opt/rollcall/rollsign`，其次 PATH 中的 `rollsign`）
- 尚未配置公钥的实例必须显式加 `--allow-unsigned` 才能只校验 SHA-256（会打印警告）；
  该参数与已有公钥/`--signature` 互斥，避免「以为验了签」
- 升级前校验 ELF 魔数与 SHA-256；当前二进制备份为 `rollcall-server.prev`
- **替换路径默认从 `--service` 单元的 `ExecStart` 推导**，不是写死的 `/opt/rollcall/...`：
  否则多实例主机上 `--service B` 配默认路径会替换掉 A 的二进制、再重启 B，
  A 悄悄跑在旧文件上而脚本报告成功。显式 `--bin` 与单元 ExecStart 不一致时直接拒绝
- 原子替换后重启 systemd 单元，最多等待 20 秒；只有 `/readyz` 返回 200 且
  版本号与目标二进制一致才算成功，否则自动恢复 `.prev` 并再次重启
- 健康检查需要 `curl` 或 `wget`，都没有时报错退出；确实要跳过请显式传 `--health ""`
- 使用 `/run/rollcall-upgrade.lock` 防止并发执行；锁里记录 pid 与进程启动时间，
  **上一个进程被 `kill -9` / 断电后遗留的锁会被自动回收**（并打印被回收的 pid），
  不会把后续升级永久锁死；锁目录刚创建、还没有 pid 文件时按「可能正在初始化」拒绝，
  超过 10 分钟才视为遗留；仅支持 Linux systemd 部署
- 脚本不会修改 unit 文件与数据目录；schema 升级后的数据回滚仍需按下方说明恢复备份

### 2. 被控端 Agent

推荐使用安装脚本的升级模式：只替换二进制，配置与 unit 保持不变。

```bash
curl -fsSL https://你的面板/agent/install.sh | sudo sh -s -- --upgrade
```

脚本会：

1. 从 `/etc/rollcall/agent.json` 读取控制端地址（**不需要节点令牌**，也不会改写配置）；
2. 从 `rollcall-agent.service` 的 `ExecStart` 找到实际二进制路径（找不到时回退 `/usr/local/bin`），
   也支持 `--upgrade /自定义/路径`；
3. 按当前架构下载，校验服务端 SHA-256 与可执行性，保留原属主后原子替换，
   旧版本备份为 `<二进制>.prev`；
4. 重启 `rollcall-agent`，若启动失败**自动回滚**到 `.prev`；没有 systemd 的容器
   会重启同一低权限用户的后台进程（日志 `/var/log/rollcall-agent.log`），同样带自动回滚。

手动回滚：

```bash
sudo rollcall-agent -rollback        # 恢复 .prev 并重启
# 或手动换回
sudo mv /usr/local/bin/rollcall-agent.prev /usr/local/bin/rollcall-agent
sudo systemctl restart rollcall-agent
```

没有配置 `-agent-dir`（安装脚本 404）时，按原来的方式替换二进制即可：

```bash
sudo systemctl stop rollcall-agent
sudo install -m 0755 rollcall-agent /usr/local/bin/rollcall-agent
sudo systemctl start rollcall-agent
```

Agent 在每次上报时通过 `X-Agent-Version` 请求头上报版本，**下一轮上报（≤30 秒）后面板即可看到**。
节点列表与详情页会在 Agent 低于控制端时标记「可升级」；老 Agent 不上报版本时显示 `—`。
该请求头对旧服务端无影响，先升级 Agent、后升级控制端也不会被拒收。

### 3. 安装脚本与分发的二进制

- `/agent/install.sh` 由运行中的服务端实时生成，**升级控制端后脚本即为最新版**，不需要单独维护；
- 使用 `-agent-dir` 时，Agent 二进制不在服务端二进制里，每个版本都要同步替换，否则新安装的节点仍会拉到旧版本：

```bash
sudo install -m 0755 dist/rollcall-agent-linux-{amd64,arm64,arm,386} \
                        /opt/rollcall/agent/
```

替换分发文件无需重启控制端，下一次 `/agent/download` 请求即生效；
`/agent/download.sha256` 会即时重新计算新文件的摘要。

### 4. 版本对应关系

| 现象 | 含义 / 处理 |
|---|---|
| 面板节点显示「可升级」 | Agent 版本低于控制端，按上面的脚本升级 |
| Agent 版本显示 `—` | 尚未上报过版本：旧 Agent 升级后等下一轮上报，或检查上报是否正常 |
| `--upgrade` 提示未找到配置 | 该节点从未通过安装脚本安装，先执行一次完整安装 |
| `--upgrade` 提示 404 / 无法下载 | 控制端未配置 `-agent-dir`，改用手动替换二进制 |
| 安装脚本提示 SHA-256 不匹配 | 下载被截断或反代返回了错误页；重试并检查 `/agent/download` 与 `/agent/download.sha256` 是否一致 |

### 5. 卸载

先备份 `data/`（账号、令牌、历史都在里面），再停止并删除。**删除数据目录不可恢复**。

```bash
# 控制端
sudo systemctl disable --now rollcall-server
sudo rm -f /etc/systemd/system/rollcall-server.service
sudo rm -rf /etc/systemd/system/rollcall-server.service.d
sudo systemctl daemon-reload
sudo rm -rf /opt/rollcall                 # 确认已备份后再执行
sudo userdel rollcall 2>/dev/null || true
```

```bash
# 被控端（每台都要执行）
sudo systemctl disable --now rollcall-agent 2>/dev/null || true
# 无 systemd 的容器改为：sudo pkill -f /usr/local/bin/rollcall-agent
sudo rm -f /etc/systemd/system/rollcall-agent.service
sudo rm -f /etc/systemd/system/rollcall-agent.service.d/*.conf
sudo rm -f /usr/local/bin/rollcall-agent /usr/local/bin/rollcall-agent.prev
sudo rm -f /etc/rollcall/agent.json /var/log/rollcall-agent.log
sudo systemctl daemon-reload
sudo userdel rollcall-agent 2>/dev/null || true
```

最后删除反向代理站点与 DNS 记录，并吊销不再使用的管理令牌。

---

## 七、项目结构

```
rollcall/
├── cmd/
│   ├── server/main.go       服务端入口
│   ├── agent/main.go        只读 Agent 入口
│   └── rollsign/main.go     发布签名工具（keygen / pubkey / sign / sign-release / verify-release）
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
│   ├── releasesig/          服务端发行二进制的 Ed25519 签名与验签
│   ├── theme/               受限主题模型与校验
│   ├── themepack/           公开页主题包发现与清单校验
│   ├── version/             构建版本号（ldflags 注入）
│   └── web/
│       ├── web.go           管理/公开 API + 内嵌静态资源
│       ├── statuspub.go     公开状态页数据：脱敏、可见性、区间聚合、服务延迟
│       ├── statuspage.go    公开页 HTML/CSS 外壳（深色优先、响应式、CSP 兼容）
│       ├── auth.go          登录 / 会话 / 退出端点
│       ├── ready.go         /readyz 深度健康检查（存储/磁盘/快照）
│       ├── static.go        内嵌静态资源 gzip 与弱 ETag / 304
│       └── dist/
│           ├── index.html   管理后台外壳
│           ├── app.js       管理后台 SPA
│           ├── style.css    管理后台样式
│           └── status.js    公开页渲染：路由、增量刷新、手绘 SVG 图表
├── deploy/                  systemd 单元与配置样例
├── docs/operations.md       备份、升级、插件、自更新与故障排查
├── docs/comparison-kpanel-2026-10-22.md  对照 KPanel 竞品页的 44 项差距与改进清单
├── .github/workflows/ci.yml gofmt / vet / test / -race / 交叉编译
└── LICENSE                  MIT
```

---

## 八、安全设计要点

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
| 服务端升级被投毒 | 升级脚本用本机公钥验签（Ed25519 绑定版本/哈希/大小），再核对 `-version` 与 `/readyz`；失败自动回滚 |
| 时序数据被污染 | 服务端覆盖 Agent 时间戳；时间倒退的样本直接丢弃 |
| 快照损坏 | 自动隔离为 `state.json.corrupt-<时间>` 并拒绝以空库启动，避免令牌/台账静默丢失 |

**明确不做的事**（这是设计选择，不是缺陷）：

- ✘ 不提供 Web 终端 / 文件管理器 / 远程命令 —— 需要时请用标准 SSH
- ✘ 不提供多租户与工单审批 —— 个人/小团队场景不需要
- ✘ 不做 PromQL 级深度可观测 —— 需要时上 Prometheus

---

## 九、已知限制

1. 未内置 ICMP ping 探测（需 raw socket 权限）；HTTP/TCP/DNS + 丢包/抖动已覆盖绝大多数场景
2. 长历史依赖本地分层聚合（默认 1h 层 365 天）；更长期归档或多实例仍需外接 TSDB
3. 单实例单进程，无水平扩展设计（40 台满环实测 RSS 约 361MB，建议 1.5–2GB 内存，容量数据见「十、容量基线」）
4. 无多用户与权限体系（单一管理员账号 + 两种 API 令牌 scope，适合个人/小团队）
5. Agent 自更新仅支持 Linux，且要求对二进制目录有写权限；控制端升级由运维执行，
   可用 `deploy/rollcall-server-upgrade.sh` 一键完成，但**不支持**远程下发升级
6. 外观主题只有固定字段；公开页主题包（`status.css`/`status.js`）与站点同源，等同管理员权限；
   通知插件是本机受信任扩展而非沙箱；两者都需要管理权限才能配置

---

## 十、容量基线

以下数据由本仓库 `bench/bench_steady.py` 实测（Windows 11 开发机 + Ubuntu 22.04 VPS，单实例回环）：
先构造"每节点 24h 原始环已满（2880 点）"的 `state.json`，再启动服务端测量，
这样才能反映稳态，而不是空环下偏乐观的数字。

| 平台 | 节点数 | state.json | RSS | CPU 核数 | /api/nodes p50/p95 | /api/status p50/p95 | 快照耗时 |
|---|---|---|---|---|---|---|---|
| **Linux VPS**（Ubuntu 22.04 / 共享 vCPU） | 20 | 3.3 MB | 195 MB | 0.031 | 6.2 / 8.4 ms | 6.6 / 10.3 ms | 307 ms |
| 同上 | 40 | 6.7 MB | 361 MB | 0.050 | 11.5 / 17.3 ms | 11.1 / 14.9 ms | 662 ms |
| Windows 11 开发机 | 20 | 3.3 MB | 143 MB | 0.011 | 12.9 / 21.3 ms | 14.9 / 24.7 ms | 111 ms |
| 同上 | 40 | 6.7 MB | 287 MB | 0.028 | 14.8 / 24.0 ms | 15.3 / 28.9 ms | 229 ms |
| 同上 | 100 | 16.7 MB | 1252 MB | 0.043 | 24.0 / 33.2 ms | 21.9 / 34.5 ms | 583 ms |

> 两个平台的内存接近，但**快照耗时差 2–3 倍**：VPS 的共享 vCPU 单核性能弱于开发机，
> 而快照是单线程的序列化工作。规划容量时请按 Linux 那一行估，别按开发机估。
> Linux 上 `VmSize`（私有内存）包含 Go 预留的地址空间，没有参考意义，只看 RSS。

**怎么读这张表**

1. **内存不能按"几十兆"估算**：每个原始点带 `mounts`/`nics` 切片，满环后约
   **5–9 MB/节点**（40 台实测 287–361 MB，100 台 1.25 GB）。
   - 40 台：1 GB 内存够用；考虑到快照 662 ms 期间的峰值，**建议 1.5–2 GB**；
   - 100 台：建议 ≥2 GB，4 GB 更稳。
2. **空环会低估一个数量级**：同样 40 台，环为空时 RSS 仅 107 MB、快照 2.2 ms，
   跑满 24h 后变成 287 MB / 229 ms。评估容量必须用满环数据。
3. **快照是唯一的周期性成本**：每 60 秒一次，40 台 229 ms（开发机）/ 662 ms（VPS）、
   100 台 583 ms。
   深拷贝阶段持有读锁，上报写入会短暂排队；在 30 秒上报间隔下无感。
4. **CPU 很低**：40 台约 0.03 核、100 台约 0.04 核（不含被监控机自身的负载）。
5. 想进一步压低常驻内存，可设 `GOMEMLIMIT=512MiB` 换取更积极的 GC，
   代价是快照期间 CPU 略升。

**快照耗时花在哪**（40 台满环 / 开发机）

v1.12 把历史点从主快照里拆成 `data/series/<id>.json.gz` 分片，并且分片按
**5 分钟**独立节奏写（不跟随 60 秒的常规保存）。实测：

| 场景 | 主快照 | 历史分片 | 合计 |
|---|---|---|---|
| **常规保存**（每 60 秒，跳过分片） | 3 KB / 1–10 ms | 不写 | **≈1–10 ms** |
| **分片写入**（每 5 分钟，40 台全脏） | 3 KB | 6.8 MB / 39 个文件 / **247 ms** | **≈259 ms** |
| 改造前（每 60 秒，整份 30 MB JSON） | — | — | 224 ms（VPS 上 662 ms） |

三点结论：

1. **60 秒的常规保存从 224 ms 降到 1–10 ms**：主快照只剩配置与当前态（3 KB），
   深拷贝从 30 MB 降到几百字节，长期持有读锁的问题彻底消失。
2. **历史写入的成本没有消失，只是被"攒"起来了**：gzip 压缩 30 MB 的总量不变，
   拆成 40 个小文件后甚至多了约 14% 的文件开销（247 ms vs 224 ms）。
   所以分片必须**降频**——每 5 分钟一次，平均摊到每次保存约 50 ms，
   且崩溃最多丢 5 分钟历史（配置与当前态从不丢）。
3. **增量在同一节奏下不起作用**：30 秒上报 + 60 秒保存意味着每次保存
   几乎所有节点都变了。真正让增量有意义的是"分片降频"本身。
   想让历史写入再降一个数量级，只能改成**追加写**（只 append 新点），
   那需要处理环形缓冲绕回与定期 compact，留作后续。

分片还顺带解决了三件事：单个分片损坏只影响那台机器的历史图（不再整份判损坏）、
删除节点时磁盘即刻清理、`state.json` 从 6.7 MB 降到 3 KB（备份/传输/打开都更快）。

分段耗时已暴露为指标：`rollcall_save_{copy,marshal,encode,write}_seconds`、
`rollcall_save_bytes`，以及分片三项 `rollcall_save_series_{seconds,bytes,files}`。

**数据布局**

```
data/
  state.json              配置 + 当前态（约 3 KB，60 秒一次）
  series/<id>.json.gz     每节点 24h 原始点（5 分钟一次，增量）
  history.json            1m/5m/1h 三级聚合
  admin.token / session.key / admin-user.json
```

升级时会自动迁移：第一次启动读旧快照（`series` 内嵌在 `state.json` 里），
第一次保存写出分片并从主快照里移除 `series`。
**回滚到 v1.11 及更早版本会读不到历史**（它们只认 `state.json` 里的 `series`），
回滚前请先备份 `data/series/`。

**复现方式**

```bash
go build -o /tmp/rollcall-bench ./cmd/server
python3 bench/bench_steady.py --binary /tmp/rollcall-bench --nodes 20 40 100 --settle 75
python3 bench/bench_capacity.py --binary /tmp/rollcall-bench --nodes 40 100 200   # 动态负载/延迟
```

> 用 `python3` 而不是 `python`：Debian/Ubuntu 默认不装 `python` 这个名字的软链，
> 照抄 `python ...` 会直接 `command not found`。

> 说明：`bench_capacity.py` 用短时高频率上报压接口（环不会跑满），
> 适合看"上报吞吐与接口延迟"；`bench_steady.py` 用预填满环的快照，
> 适合看"内存与快照成本"。两者结论互补，不能互相替代。

---

## 十一、后续路线

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
| ~~P10~~ | ~~监控闭环：服务/证书告警、到期提醒、流量配额、续费日历~~ ✅ |
| ~~P11~~ | ~~告警指标扩展、通知降噪（免打扰/摘要/模板/报告）、DNS 与丢包抖动、TOTP、范围化 API 令牌~~ ✅ |
| ~~P12~~ | ~~跨节点对比与收藏、加密配置备份、运行报告、公开页地图/活动流、多挂载点/网卡/磁盘 I/O 采集~~ ✅ |
| ~~P13~~ | ~~控制端一键升级/回滚脚本（校验哈希 + 健康检查 + 自动回滚；v1.14 起支持 Ed25519 发行验签）~~ ✅ |

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

*只读 MCP 服务器*（v1.21.0 起，`rollcall-mcp`，随发布包分发）

让 Claude Desktop 等 MCP 客户端直接查询机群状态。stdio 传输、换行分帧 JSON-RPC 2.0，
零第三方依赖；它是面板的普通 HTTP 客户端——**只读、不下发任何指令、不接触节点**。
7 个工具：`get_overview` / `list_nodes` / `get_node` / `get_node_series` / `list_services` /
`list_alerts` / `list_assets`，全部映射到 `authReadonly` GET 端点。

令牌在面板「设置 → API 令牌」创建：`scopes=["read"]`，`paths` 限定到上述端点前缀。
客户端配置示例：

```json
{
  "mcpServers": {
    "rollcall": {
      "command": "/path/to/rollcall-mcp",
      "env": { "ROLLCALL_URL": "http://127.0.0.1:19080", "ROLLCALL_TOKEN": "rct_…" }
    }
  }
}
```

也可用命令行参数 `-url` / `-token`。stdout 是协议通道，日志一律走 stderr；
令牌明文会存进 MCP 客户端配置，请用只读作用域令牌而非管理令牌。

---

## 十二、HTTP API（v1 分页约定）

历史类数据（告警历史 / 通知投递队列 / 管理审计）会持续增长，`/api/v1/` 下的接口
提供**服务端分页 + 统一响应信封**，供脚本与第三方集成使用。

**统一信封**

```json
{
  "items": [],
  "total": 137,
  "limit": 20,
  "offset": 0,
  "hasMore": true
}
```

- `total` 是**筛选后的匹配总数**，不是当前页条数——前端据此算总页数；
- `hasMore` 等价于 `offset + len(items) < total`，客户端可只依赖它做「下一页」；
- `limit` 默认 20、上限 200；超过上限按上限截断而**不报错**（分页接口的职责是限制单次响应体积）；
- `offset` < 0 归零；越界返回空 `items`，但 `total` 不变（前端据此把页码夹回最后一页）。

| 端点 | 说明 | 主要参数 |
|---|---|---|
| `GET /api/v1/alerts` | 告警历史（附 `active` / `silences` / `notify` / `now`） | `limit` `offset` `state=all\|active\|resolved` `q` |
| `GET /api/v1/admin/notify/jobs` | 通知投递队列（附 `stats`） | `limit` `offset` `status=pending\|sent\|failed\|suppressed` |
| `GET /api/v1/admin/audit` | 管理审计日志（附 `stats`） | `limit` `offset` `range=24h\|7d\|30d\|90d` `since` `until` `actor` `action` `q` |

鉴权与同名老端点一致：`/api/v1/alerts` 接受只读会话或令牌，
另两个需要管理会话或 `X-Admin-Token`。

**为什么保留老端点**：`/api/alerts`、`/api/admin/notify/jobs`、`/api/admin/audit`
的响应形状（`items` / `entries` / `jobs` / `stats`）已被旧版前端与外部脚本依赖，
改形状属于破坏性变更。老端点的语义是"取最近 N 条"，没有 `offset` 概念；
新端点才提供分页。两者数据源与排序完全一致。

**为什么需要分页**（真实盲区，不是洁癖）：

- 告警历史在内存保留 500 条（`store.AlertRetainMax`），老接口只给最近 200 条 → **300 条不可达**；
- 通知队列上限 2000 条（`store.MaxNotifyJobs`），配置页固定取 100 条 → **1900 条不可达**；
- 排障时"上周那条告警到底发出去了吗"过去只能去翻 `journalctl`，现在可直接翻页。

**排序稳定性**：告警按"追加顺序逆序"（创建时间新→旧）；投递队列按
"状态优先级 → 创建时间倒序 → ID 倒序"。末位用 ID 兜底是为了同一秒内的多条记录也有
确定顺序——否则相邻页会重复或遗漏，这是分页接口最典型的缺陷（`TestV1AlertsSortStable` 锁住它）。

**检索在后端做**：`q` 在分页之前过滤，`total` 反映全量命中数。若先取一页再在客户端过滤，
用户会看到"只有几十条历史"的错觉——这正是改造前的实际表现。

## 十三、开发与验证工具

`tools/` 是可复跑的验证工具（入库、零依赖、只读公开接口；凭据走环境变量）。

| 工具 | 用途 | 用法 |
|---|---|---|
| `tools/check-contrast.mjs` | **WCAG 2.1 对比度自检**：解析 `style.css` 的深浅两套变量，正文/语义色 ≥4.5（AA 硬闸），辅助文字与强调色 ≥3（提示）。已挂 CI | `node tools/check-contrast.mjs` |
| `tools/audit-headers.mjs` | **线上安全头与 TLS 回归**：CSP 脚本白名单、HSTS 时长、nosniff、点击劫持防护、明文跳转、证书剩余天数与链可信度 | `node tools/audit-headers.mjs [url]` |
| `tools/health-check.mjs` | **运行状态巡检**：端点可达性、**服务端时钟偏差**（>120s 会让上报签名整批被拒）、节点在线构成、**上报新鲜度**（单台超时=节点问题，全台超时=服务端停摆）、24h 可用率、活跃告警、历史层天数。不需要凭据 | `node tools/health-check.mjs [url] [--json]` |
| `tools/ui-session.mjs` | **管理面板校验会话**：cookie 持久化到 `.auth/cookies.json`（登录一次复用），失败自动截图，统一 `goto/eval/shot/ok` 封装；`waitStableView({selector})` 等页面稳定（SSE 会触发整页重渲染，固定 sleep 会撞上中间态造成假失败） | 作为库使用：`openSession()` / `withSession()` |
| `tools/ui-check.mjs` | **UI 关键不变量**：分页器生效、可见行 ≤10、「自定义列」按钮、详情页系统信息是否**真的可见**、公开页渲染与无横向溢出 | `RCP_PASSWORD=... node tools/ui-check.mjs` |

凭据约定：`RCP_PASSWORD` 环境变量，或 `.auth/password`（该目录已 gitignore）。
`tools/` 与 `scripts/` 的区别：前者入库、可给 CI 与开源用户复跑；后者是个人的运维临时脚本（含真实主机与凭据，不入库）。

当前基线（2026-10-28）：对比度 fail=0、安全头 12/12、UI 不变量 28/28、健康巡检 10/10。
巡检工具的分工：`health-check` 看「数据是否在动」，`audit-headers` 看「配置还对不对」，
`ui-check` 看「界面关键不变量」——三者互补，任一为红都说明生产有问题。

## 十四、实际部署验证

### v1.43.0（2026-10-31，Nezha 一键迁移 + 接入命令修复）

**从 Nezha 一键迁移**（替代 Nezha 的第一步）：

- 新端点 `POST /api/admin/nodes/import/nezha`：填 Nezha 主控地址与管理令牌，服务端拉取
  `/api/v1/server` 清单，映射成批量导入规格（name / country_code→country / note）后
  走与粘贴导入**完全相同**的落库路径（applyNodeImport：校验、重名跳过、整批落盘、
  接入命令生成）——两条迁移路径的口径只维护一份
- 节点页新增「⇩ 从 Nezha 迁移」弹窗，结果复用批量导入的结果弹窗（每台的接入命令 +
  跳过原因）；Nezha 令牌只在本次请求内存中使用，不落盘、不进审计、不回显到响应
- 宽松映射：Nezha 各版本 host JSON 键大小写不统一（CountryCode / country_code），
  按不区分大小写查找；重名/空名跳过且不覆盖既有台账
- 不导入 Nezha 的任何监控数据——指标由 Rollcall Agent 重新采集

**修复一个存量接入命令 bug**（b1419b1 起，影响批量导入/Nezha 导入的全部命令）：
`agentInstallCommand` 的格式串多写了一段路径，生成的命令 curl 请求
`install.sh/agent/install.sh`（404，安装必败）。E2E 验证弹窗时抓到；修复后路径恰好
出现一次并有测试断言锁定。面板「节点接入」单台命令不受影响（前端另行拼接）。

### v1.42.0（2026-10-30，隐私脱敏 + 备份含监控历史）

**隐私（开源仓库清理）**：本仓库是公开项目，此前多轮把作者自己的部署信息带了进去，本轮全部清除——

- 移出跟踪：`docs/部署与验证报告-20261017.txt`、`docs/借鉴评估-20261027.md`（含生产 IP、
  主机配置与个人拓扑；文件保留本地，`.gitignore` 加规则防再犯）
- 全库脱敏：生产 IP → 文档示例 IP（203.0.113.x）、个人域名 → example.com、
  真实节点昵称（各 VPS 商家+城市组合、家宽节点等，可拼出完整资产清单）→ 虚构名；
  涉及 README、三份对比文档、运维手册、发布清单与测试断言
- `seed.go` 演示数据此前 1:1 复刻真实机群节点名，全部改为虚构（demo-* / 云甲…）；
  `/monitor` 的 mock 演示数据同样虚构化并重建产物
- `tools/` 默认探测目标从作者生产实例改为 `127.0.0.1:19080`——开源工具不应默认打某个人的面板
- 注：git 历史中的旧 tag 仍含早期提交的这些信息，如需彻底清除需 filter-repo 重写历史（破坏性，单独决策）

**备份 v3：含聚合监控历史**（此前备份只有 7.7KB 配置，磁盘故障会丢掉 365 天历史）：

- 备份格式 v3 新增 `HistoryGz`：三级聚合历史的 gzip 快照，与 `history.json` 同一编解码路径，
  无损往返；控制端磁盘故障后可用率/负载曲线可整体恢复
- 还原语义（拍板）：配置与历史**整体替换**为备份时间点状态——不做按节点合并，宁要一段
  诚实的空窗不要一段交错出的假数据；原始 24h 环形不在备份内（恢复后 24h 随上报自愈，
  换取备份体积不随环膨胀）；告警记录与通知队列不在备份内（既有语义）
- 安全栅栏：历史快照 schema 高于当前服务端时**整个导入被拒绝**（先升级再还原），不会出现
  "配置换掉、历史没动"的半还原；v2 及更早备份照常导入（历史保持还原目标现状）；
  无聚合历史时导出不带 HistoryGz（还原端不清空目标）
- 还原 API 响应新增 `historyRestored`，UI 说明文案同步；自动备份沿用同一路径，
  单份体积约等于 `history.json`（几 MB），14 份保留 ≈ 百 MB 级
- 测试：v3 往返（三层聚合+数值来源）、v2 兼容、schema 拒绝后状态原样、空历史缺省 4 组；
  23 包全绿

### v1.41.2（2026-10-28，`/monitor` 按成品收口：去掉内部说明与数据源痕迹）

页面之前带着"作者视角"的痕迹，不像交付给使用者的东西，这版全部去掉：

- **删除页面底部的「数据说明」折叠区**（图表内容 / 字段映射 / 刷新机制 / 交互反馈四张表）——
  那是开发文档，不该出现在看板上。
- **删除顶栏的数据源下拉**（「数据源：模拟 / 实时接口」）。数据源由部署位置决定，同源即真实
  数据，不该让使用者选；`localStorage` 也不再持久化它——否则 https 部署里读到残留的 `mock`
  就会展示演示数据，那是最坏的情况。
- **心跳改说人话**：`更新于 12 秒前 · 26s 后刷新`，不再写「实时接口 / 推送已连接」。
- **详情页去掉接口路径**：缺数据时提示「暂无历史数据」，不再列 `/api/status/node/{hid}?range=`。
- **列表整列为空就不渲染该列**：公开数据不提供地区与系统时，原本会出现两列铺满「—」；
  现在列定义支持 `optional`，整列无值即省略（11 列 → 按数据在 9 列间自适应）。
- **一处重复渲染**：CPU/内存/磁盘缺值时 `miniBar` 已吐「—」，外层又拼一个，会显示成「— —」。

顺带修掉三个观感问题：详情页右上角分组名为空时留下悬空的「prod ·」；环形图中心的百分号
只有 10px 且贴着基线，看着像小数点（改 12px + 轻移）；卡片底部的两个速率没有任何方向
标识（补 ↓ / ↑）。离线单文件预览仍可用，此时走内置演示数据，但界面同样不出现「模拟」字样。

### v1.41.1（2026-10-28，修复详情页被轮询反复重建）

**现象**：在 `/monitor` 详情页切到「7 天」后，图表停在骨架屏不再出现；探针显示
同一个 `?range=7d` 被请求了 **3 次**。

**根因**：`render()` 在每次数据刷新时都会重建详情页结构并重新取序列。进入详情后，
首次渲染 + SSE 推送 + 轮询各触发一次，三个 `mountDetailCharts` 抢同一个 `detailToken`；
最后一个拿到 token 时，`#charts` 已被下一次渲染换成新节点，而它持有的
`box` 引用指向已脱离 DOM 的旧节点 —— 写入成功但页面上看不见，表现为"停在骨架屏"。

**修复**：详情页引入结构签名（`节点 ID | 区间`），自动刷新命中同一签名时直接跳过重建；
手动刷新（按钮 / `R` 键）与切换区间仍然重建 —— 那两个动作的语义就是"我要看最新的"。

**验证**：进入详情 **1 次**序列请求（原 3 次）；等待一轮 35 秒的自动刷新后
曲线仍在、骨架 0 个、**新增序列请求 0 次**；手动刷新恢复 1 次请求并重绘。

> 这条与 v1.39.2 的「区间切换被过期轮询覆盖」是同一类问题：轮询与用户操作抢同一块 DOM。
> 教训是一致的——**定时刷新只该更新数据，不该重建结构**。

### v1.41.0（2026-10-28，新增 `/monitor` 控制台式视图）

在公开页之外提供第二种呈现方式：`/monitor`，条形图 / 环形图 / 列表三视图 + 内页详情，
与 `/status` 共用同一份数据与同一道访问门槛。

**数据来源（严格只用公开接口，不读管理接口）**
- 列表与卡片：`GET /api/status`
- 内页详情曲线：`GET /api/status/node/{hid}?range=`（返回 `series.{cpu,mem,disk,netRx,netTx}`）
- 实时刷新：`GET /api/status/stream`（事件只带版本号，收到即重取；轮询作为兜底）
- 最近事件：`/api/status` 的 `activity` 字段，不额外发请求

**几个刻意的取舍**
- **时间范围只保留 24 小时 / 7 天 / 30 天**：公开详情接口只认这三档，其它值静默回落 24h。
  若界面还摆着「1 小时」，用户选了却拿到 24 小时的数据、图看起来没变，比少一个档位严重得多。
- **硬件信息不填**：公开接口不下发 OS / 内核 / CPU 型号，面板就不显示这几格，
  不用本地资产台账去"补"——同名节点在别人的部署上会被贴错标签。
- **负载无历史就明说**：接口只给负载的当前值，详情页因此只显示数值并注明「没有历史序列」，
  不画一条用当前值撑起来的假曲线。
- **缺字段显示「未公开」而非 0**：0% 和"没有数据"是两件事。

**工程细节**
- 拆成 `dist/monitor.{html,css,js}`：CSP 是 `script-src 'self'`，内联 `<script>` 会被静默拦截
  （页面停在骨架屏且不报错）。单文件源在 `monitor-panel/index.html`，`node monitor-panel/build.mjs`
  生成产物；脚本会断言产物无 CRLF、无 U+FFFD（go:embed 嵌原始字节，行尾变化会让 digest 漂移）。
- **首屏不闪白**：`data-theme` 无法在 CSS 生效前由脚本设置（内联脚本被禁），
  因此暗色变量额外用 `@media (prefers-color-scheme: dark)` 提供一份默认。
- 主题切换不采用 auto→light→dark 三态循环：系统是浅色时 auto 与 light 视觉相同，
  点一次看不出变化，用户会以为按钮坏了。现在按当前可见外观取反。
- 搜索框 180ms 防抖 + 重绘后恢复焦点与光标位置（否则每敲一个字就失焦）。

**验证**：本地 demo 实例上三视图 / 排序 / 筛选 / 详情 / 三档区间 / 深浅主题全通过，
**零 JS 异常、零资源 404**；两套主题对比度实测（暗色最低 4.88、浅色 4.55），
用 WCAG 公式计算而非目测。

### v1.40.0（2026-10-28，Agent 采集能力上报：让"没有数据"有解释）

**问题**：本机群 13 台 VPS **全都没有 hwmon 温度传感器**，面板因此从来不显示温度卡片。
但界面对"没有温度"是完全沉默的，用户无法区分三种完全不同的情况：

| 实际情况 | 该做什么 | 改造前 |
|---|---|---|
| 机器没有传感器 | 什么都不用做（正常） | 一片沉默 |
| 有传感器但 Agent 读不到（权限/内核） | 需要排查 | 一片沉默 |
| Agent 版本太旧，根本没采 | 升级即可 | 一片沉默 |

**做法**：Agent 启动时探测一次可选能力，随每次上报以请求头 `X-Agent-Caps` 发出；
服务端解析后存到节点上；面板据此陈述事实。

- 新包 `internal/agentwire/caps.go`：`HeaderAgentCaps` / `CapsNone` / `NormalizeCaps` / `ParseCaps`。
  **两端共用同一份解析**——这类枚举字段最容易在 Agent 与服务端各写一套，
  然后在对大小写、空项、重复项的处理上分叉。
- **三种状态必须可区分**：头缺席 = 旧 Agent（未知）；`none` = 已探测且确实没有；`temp` = 有传感器。
  少了 `none` 这个哨兵，"空清单"会被当成"没发头"，三种情况又退回成一团。
- 探测判据是"**是否存在 `temp*_input` 文件**"，而不是"这次读数是否 >0"：
  虚拟化环境的传感器可能恒返回 0（采集侧会当无效值丢弃），但文件存在说明机器有传感器，
  不该被报成"没有传感器"。
- 存储：`SetNodeCaps` 只在变化时落盘（能力在进程生命周期内不变），且**拒绝空值**——
  旧 Agent 的无头上报绝不能把已知能力降级成未知。
- UI：温度卡片缺席时显示一行中性说明（`--fg-3`，非警告色）：
  「本机没有可读的 hwmon 传感器（大多数 VPS 如此，属正常）」/
  「Agent 报告有传感器但该区间无读数（检查是否可读）」/
  「Agent 未上报采集能力（v1.40.0 起支持），暂无法区分」。

**为什么用请求头而不是上报 JSON 字段**：服务端用 `DisallowUnknownFields` 解码，
给 body 加字段会让**旧服务端**整份拒绝（升级顺序被迫反转）。请求头对旧服务端透明。
这也让升级可以先推 Agent、后推服务端。

**测试**：`agentwire/caps_test.go`（规整/解析/截断/判定）、
`web/caps_test.go`（三条上报路径 + **空值不降级** + 重启后仍在）、
`collect/temp_linux_test.go`（`temp*_input` 存在性判据）。
按惯例注入缺陷验证：去掉"拒绝空值"后 `TestAgentCapsPersistAndNotDowngraded` 失败。

### v1.39.3（2026-10-28，检索重置分页 + 拆分共用的分页状态）

**两个缺陷，都出在"分页状态"这一层**

1. **检索不重置页码**：在告警页翻到第 3 页再输入检索词，结果集变短后被"夹到最后一页"，
   看到的是**最旧的那几条匹配**——用户会以为检索结果排序错了。
   检索词变化时现在会把三个分页计数器（概览异常节点、告警记录、审计）统一归位到第 1 页。
2. **一个计数器被两个列表共用**：`state.alertPage` 同时服务「概览 → 正在告警的节点」分页
   与「告警 → 历史记录」分页，在一处翻页会让另一处从第 3 页开始。
   新增独立的 `state.alertRecPage`，两者彻底分离（顺带清掉了 `state` 字面量里
   **重复声明两次**的 `alertPage` —— 那正是这个混淆留下的痕迹）。

**为什么改这里**：上一版给告警记录加了服务端分页，分页从"前端切片"变成"真翻页"，
页码就不再是纯粹的显示状态，而是一个会跨视图泄漏的变量。同类问题必须在引入后立刻收敛。

**验证**：`tools/ui-check.mjs` 新增「检索后回到第 1 页」断言（先翻到第 2 页、再输入检索词、
断言分页器显示第 1 页，随后清空检索框还原状态）。基线 22/22。

### v1.39.2（2026-10-28，修区间切换被过期轮询覆盖）

**现象**：详情页点「30 天」，地址栏已变成 `?range=30d`，但图表仍停在「1 小时」——
且**间歇性**：同一操作有时成功、有时卡住；没有任何 JS 报错；直接访问
`#/nodes/<id>?range=30d` 又完全正常。

**根因**：详情页有 5 秒兜底轮询 `tickDetail`，它在发起请求时**捕获当时的区间**，
取完数据后无条件 `render()`。用户在请求在飞的 5 秒窗口内点切换时：

1. 点击 → `state.range = 30d` → 重新取数并渲染 30d；
2. 那轮更早出发的 tick 此时返回 1h 的数据 → `render()` → 覆盖成 1h。

窗口太窄时不会触发，所以表现为偶发；本地演示实例（单节点、数据少、响应快）
复现不出来，只有生产（13 台、30 天 2200+ 点）才撞得上。

**修复**（两处，均为主干路径收敛）

- `tickDetail`：请求返回后先比对 `range !== state.range`，**区间已被切换就丢弃这一轮**。
  过期结果不参与渲染，下一轮 tick 自然按新区间取数。
- 区间按钮改走 `render()` 而不是直接 `viewNodeDetail()`：让路由重新解析 URL 参数，
  与「刷新页面 / 分享链接」走同一条路径，不会出现地址栏与界面两套状态并存。

**验证**：本地连续切换 `30d→7d→24h→6h→1h→30d` 六次全部一致（`on` 高亮与 URL 参数逐项匹配，
且分位带只在有 5m 桶的区间出现）；修复前的对照组在本地**无法复现**（竞态窗口过窄），
因此结论来自生产上的间歇性证据 + 代码路径分析，而非"对照组失败"。

### v1.39.1（2026-10-28，修复 p95 大于峰值 + CPU 分位带可视化）

**发现方式**：v1.39.0 上线后直接查生产 30 天序列，得到
`cpu=0.2 / p50=5.0 / p95=9.5 / max=0.7` —— **p95 比峰值还大**，逻辑上不可能。
这是我自己上一版引入的缺陷，用户一眼就能看出问题，必须修。

**两个叠加的原因**

1. **首档太宽**：直方图档位是等距 10 档（0–10、10–20…），一台长期 0.2% 的空载机器
   所有样本都落进首档；档内插值假设"档内均匀分布"，于是 p50 被估成 5.0、p95 估成 9.5。
2. **估算没有上界**：`percentile()` 不知道同桶的精确峰值，插值结果可以越过它。

**修复**

- 档位重新设计（**档数不变，内存一分没多**）：`1/2/5/10/20/40/60/80/90/95/98/100`。
  两端加密（0–1、1–2 能区分 0.2 与 1.8）、中段放宽（10–40 之间粗一点无所谓）、
  饱和段细分（80–90/90–95/95–98/98–100 才是容量判断关心的区间）。
- `percentile(q, upperCap)` 用**同桶精确 `CPUMax` 封顶** → `p50 ≤ p95 ≤ max` 恒成立。
- **`historySchemaVersion` 3 → 4**：档位边界是存储语义的一部分，schema 3 的分档计数
  在新边界下读数无意义，因此加载旧文件时**丢弃直方图、保留 avg/max/用量**
  （否则一次升级抹掉 30 天历史，那是更严重的倒退）。分位数在 30 天内自然重建。

**同时完成分位带可视化**（原计划的下一步）：CPU 图在 5m/1h 区间画出
**p50–p95 的阴影带**（线是均值、阴影上沿是较差的 5% 样本），悬停读数追加
`p50–p95 x–y`，标题旁显示图例「阴影 = 桶内 p50–p95」。
缺直方图的桶不补 0，而是按**连续区段分段绘制** —— 补 0 会画出一条从 0 起算的假阴影；
把两端直接连起来则会跨过空缺画出错误斜带。

**测试**：`cpuhist_test.go` 扩到 10 例，新增
`TestCPUHistIdleMachineNotOverstated`（空载机器回归）、
`TestCPUHistPercentileBoundedAndMonotonic`（200 轮随机分布属性测试：
`0 ≤ p50 ≤ p90 ≤ p95 ≤ max`）、
`TestHistorySchema4DropsLegacyHistograms`（迁移时直方图丢弃但 avg/max 保留）。
按惯例做了有效性验证：去掉封顶后 3 条用例失败（`p95 = 0.95 超过峰值 0.70`），恢复即通过。

### v1.39.0（2026-10-28，历史类列表服务端分页 + API v1 统一信封）

**问题（真实盲区）**：告警历史在内存保留 500 条、老接口只给最近 200；通知队列上限
2000 条、配置页只取 100。更早的记录在界面上完全不可达；而页面上的"总数"显示的是
**已取回条数**（200 / 100），用户会误判历史规模。

**后端**

- `Store.AlertsPage(AlertPageQuery)`：offset 分页 + 状态筛选 + 检索，**筛选发生在分页之前**
- `Store.NotifyJobsPage(NotifyJobPageQuery)`：offset 分页 + 状态筛选
- 抽出 `snapshotNotifyJobsLocked`，让 `NotifyJobs` 与分页版共用同一套排序（否则两处排序键不一致会导致翻页错位）
- 新增 `/api/v1/{alerts,admin/notify/jobs,admin/audit}`：统一信封
  `{items,total,limit,offset,hasMore}`，默认 20 / 上限 200
- `AlertRetainMax`(500)、`AuditQueryMaxLimit`(200) 由散落字面量提为常量
- 老端点一字未改；`TestLegacyEndpointsUnchanged` 锁住其响应形状（缺字段即失败）

**前端**

- 告警页改服务端分页：检索词交给服务端，KPI 与区段标题显示**真实总数**（"可翻页至最早记录"）
- 设置页与告警页新增「全部投递记录」：分页 + 按状态筛选的弹窗，队列 2000 条可达

**测试**

- `internal/web/apiv1_test.go` 7 例：信封、翻页、越界、末页 hasMore、排序稳定性、
  状态/检索筛选、未鉴权 401、老接口兼容
- `tools/ui-check.mjs` 新增 6 条断言（页面总数、单页 ≤10、入口存在、信封字段完整、
  limit 约束、hasMore 与 total 一致）
- 按项目惯例做了**有效性验证**：把 `hasMore` 改成恒 false → 断言失败，恢复后通过

### v1.38.0（2026-10-27，部署矩阵补齐 + 发版清单固化）

来源：`docs/借鉴评估-20261027.md` 的 P0-4 / P0-5（对照 `Argus/deploy/` 与 `Argus/docs/checklist.md`）。

- **修复 Dockerfile 工具链残留**：`FROM golang:1.24-alpine` —— v1.35.2 升级工具链时
  漏了容器锚点，而 `go.mod` 已要求 1.27.3，**容器构建会直接失败**。改为 `golang:1.27-alpine`；
  同步修掉 CI 里「钉死在 1.24.0」的过期注释。
- **`deploy/docker-compose.yml`**：命名卷（继承镜像内 `/data` 属主，免 chown）、
  `TZ`、`-trusted-proxies 172.16.0.0/12` 预设（否则 Cookie 无 Secure、审计 IP 全是网关），
  端口只绑 `127.0.0.1`；不含 healthcheck（distroless 无 shell/curl，用 CMD 自检只会
  误报健康），改为注释指引从宿主探 `/readyz`。
- **`deploy/nginx.conf`**：TLS 终结 + HTTP 强制 308 + `proxy_buffering off` 与
  `proxy_read_timeout 3600s`（SSE 必需）+ 明确写出「不支持子路径部署」并附等价 Caddy 配置。
- **`deploy/rollcall.env.example`**：只含真实生效的两个环境变量
  （`ROLLCALL_ADMIN_PASSWORD` / `ROLLCALL_ADMIN_EMAIL`），并附常用**启动参数**速查——
  避免把 flag 当环境变量写进 EnvironmentFile（Go 的 flag 包只读命令行，写了也不生效）。
- **`docs/release-checklist.md`**：发版 DoD 七段 30 余项，每项给命令与期望值，
  含本地自检（gofmt / vet / test / JS 语法 / 对比度 / **U+FFFD 计数** / **CRLF 计数**）、
  digest 一致性、签名、部署、Agent 升级（含上海/成都/绿云三种特殊通路）、
  上线回归、收尾，以及六条常见失败的处置表。

### v1.37.4（2026-10-27，升级弹窗不再缺目标版本）

v1.37.3 只改了取值来源，仍不生效——根因更深：**`serverVersion` 只在详情接口
（`/api/nodes/:id`）返回，列表接口（`/api/nodes`）不含该字段**，而 `state.serverVersion`
只在设置页加载。所以直接打开节点页时两处都是空。

修法：弹窗改为 async，缺目标版本时补一次详情请求（单台、走浏览器缓存）；
仍拿不到则退化为「升级后与控制端版本一致」，不写死会误导的文案（旧文案把**当前版本**
标注成「控制端版本」）。

> 另修一处写入事故：用 `python -` heredoc 写含中文的 JS 时出现 3 字节替换字符
> （「控」变成 3 个 U+FFFD 替换字符）。此后**含中文的改动一律走编辑工具**，python 只做纯 ASCII 结构改写；
> 并在提交前用 `U+FFFD` 计数做一次断言。

### v1.37.3（2026-10-27，升级弹窗显示目标版本）

修复 v1.37.2 的小缺陷：直接打开节点页时 `state.serverVersion` 从未加载，弹窗只显示
「当前 v1.36.0（控制端版本）」而缺目标版本。改取节点自带的 `serverVersion`
（`/api/nodes` 每个节点都返回），回退到 `state.serverVersion`。

> 教训：README 里的反引号片段不能交给 shell 双引号包裹的 `python -c` 写入 ——
> bash 会先做命令替换，把 `` `...` `` 执行成空串。改文档一律用编辑工具。

### v1.37.2（2026-10-27，补上「可升级」的实际入口）

**问题**：节点表会给版本落后的 Agent 打「可升级」徽标，但没有任何可执行入口——
徽标是死的，操作列只有「编辑/接入」，而「加入升级清单」只是**进度台账**
（创建批次、按版本自动算完成数），本身不产生命令。用户看到标签却无处可点。

**根因**：升级通路一直存在（`install.sh --upgrade`：脚本内 `uname -m` 识别架构、
下载、校验 SHA-256、替换二进制、重启），但界面上从未把它暴露出来。

**修复**：
- 「可升级」徽标改为**可点击**（`.badge-click` 手型 + 悬停下划线），title 说明
  「Agent vX 低于控制端 vY —— 点击获取该机器的升级命令」
- 操作列在可升级时新增**「升级」按钮**（主色，与次要操作区分）
- 弹窗内容：版本对比、可复制的单行命令、脚本行为说明，并明确
  「多台建议先用升级清单建批次做跟踪」

**为什么不做「一键远程升级」**：本产品的硬边界是 Agent 只读、单向、服务端不下发
指令（攻击面从任意 RCE 降为「只能伪造指标」）。升级与接入同理，只提供在目标机器
上执行的命令。这条边界不用功能换。

### v1.37.1（2026-10-27，CSV 导出补 CPU p50/p95 列）

`/api/export/metrics.csv` 在末尾追加「CPU p50」「CPU p95」两列（追加而非插入，
避免按列位置解析的脚本错位）；无直方图的老桶留空而不是写 0（0 会被误读成
「使用率为 0」）。

### v1.37.0（2026-10-27，CPU p95 分位数：让"均值骗人"失效）

**动机**：容量判断看均值会被抹平。极端的例子是 94 个 20% + 6 个 90% ——
均值只有 24.2% 看着很闲，p95 却是 90（已接近饱和）。告警与容量规划需要尾部视角。

**为什么不用 t-digest**（原借鉴方案见 `docs/借鉴评估-20261027.md` P0-1）：
本项目历史桶有硬内存约束——每桶 64 字节、40 节点满保留期约 110 万桶、常驻约 70MB。
t-digest 单桶 100–200 字节会让常驻内存翻倍以上，而这里只需回答一个是非判断
（"这个桶里 CPU 是否接近饱和"）。改用 **12 档非均匀直方图（24 字节/桶）**：
60% 以下每档 10%（±5%），60% 以上每档 5–10%（±2.5~5%）——容量判断关心的正是高端区分。

**实现**：
- `internal/store/cpuhist.go`：`cpuHist` 12 档计数 + 档内线性插值求任意分位，可合并
- **只挂 5m / 1h 两层桶**（覆盖 30 天 ~ 365 天）：1m 与 raw 样本量小，前端直接精确算
- 三层桶各自从原始上报累加（非层间 rollup），因此每层独立 observe，无需 merge
- `model.HistoryPoint` 新增 `cpuP50` / `cpuP95`（`omitempty`，0 = 无摘要）
- 前端 CPU 图表统计行：`均值 X · 峰值 Y · p95 Z`；聚合区间取**各桶 p95 的最大值**
  （关心最差时段的尾部水位，而非被空闲期拉低的平均尾部），raw/1m 走前端精确分位
- **`historySchemaVersion` 2 → 3**：桶结构变了必须升版本，否则新代码读旧桶无法区分
  「无摘要」与「摘要为 0」

**已知边界（不是缺陷）**：极端偏斜（如 99 个 20% + 1 个 95%）下 p95 在数学上就等于
20（第 95 个样本本来就在 20 档），尖峰只能由 `cpuMax` 或 p99 体现——这是分位数的
定义，与直方图精度无关。升级后旧桶无直方图，前端自动回退到自算，随新上报逐步变精确。

**测试**：`internal/store/cpuhist_test.go` 7 个用例（精度、尖峰、空值、越界夹取、
合并、JSON 往返、端到端 p50/p95 + 1m 层不带摘要）。

### v1.36.0（2026-10-27，横向借鉴落地：三件验证工具 + 一处配色修正）

来源：`docs/借鉴评估-20261027.md`（对照 Argus / panelOps / github-trends-site / bxait-pentest 等 12 个项目）。

- **新增 `tools/`（4 个零依赖脚本，入库可复跑）**
  - `check-contrast.mjs`：WCAG 对比度自检，**已挂 CI**（正文/语义色 ≥4.5 硬闸，辅助色 ≥3 提示）
  - `audit-headers.mjs`：线上安全头 + TLS 证书回归（CSP/HSTS/nosniff/点击劫持/明文跳转/证书天数与链）
  - `ui-session.mjs`：面板校验会话（cookie 持久化复用、失败自动截图），凭据只从 `RCP_PASSWORD`/`.auth/password` 读
  - `ui-check.mjs`：UI 关键不变量（分页、可见行数、自定义列按钮、详情页系统信息可见性、公开页渲染）
- **修复配色不达标**（自检工具首次运行即发现）：浅色主题 `--fg-3`（占位符/表头/图注）
  在页面底上仅 **2.36:1**，低于 WCAG 小字 3:1 → 调整为 `#868f9c`（3.02 / 3.27），同色相、视觉差异极小
- **消除验证脚本的密码落盘**：过去每个 UI 校验脚本都重登并把面板密码写到 `scripts/rcpwd.txt`；
  现在 `.auth/`（cookie + 密码，0600）已 gitignore，登录态跨脚本复用
- **评估文档自身修订**：撤销「通知凭据脱敏」一项——复核确认 `Redacted()/mask()/isMasked()`
  早已实现且带测试，原判断因 grep 范围过窄而错误（见报告第六章修订记录）

基线：对比度 26/26、安全头 12/12、UI 不变量 11/11。

### v1.35.6（2026-10-27，表格列显隐按钮定名「自定义列」）

表格增强的列显隐入口原标题为单字「列」，无法自解释。最终定名 **「自定义列」**
（+ 图标 + 悬停提示「选择要显示的列」），与腾讯云等主流控制台命名一致。

### v1.35.5（2026-10-27，表格「列」按钮首次改名）

### v1.35.4（2026-10-27，修复详情页「系统信息」值不可见）

**现象**：节点详情页右侧「系统信息」卡片只有标签、值全部空白（同一卡片的「资产」
区块正常，曲线数据也正常）。

**根因**（CDP 实测定位，非数据问题）：`.info-list` 是 `display:grid` 但未声明列，
隐式 `auto` 列被列内 item 的 **min-content** 撑到约 700px —— min-content 被
`.info-v { white-space: nowrap }` 里的长值（64 位上报签名、完整 OS 名）拉高；
`.info-row` 用 `space-between`，值被推到行尾。侧栏仅 268px，于是值落到
**屏幕外**（实测 rowW=697 / vRight=1787，视口 1382），标签留在左侧可见——
视觉上就是「一片空白」，而数据其实全在。

**修复**（三处，注入 CSS 先验证再落地）：
- `.info-list` 显式 `grid-template-columns: minmax(0, 1fr)`（列宽=容器宽）
- `.info-row` 加 `min-width: 0`（grid item 允许收缩）
- `.info-v` 加 `flex: 1 1 auto; min-width: 0`（占剩余宽度，长值 ellipsis 截断）
- 顺带把 `.info-row` 的 `align-items: baseline` 显式化（值/标签基线对齐）

修复后实测：rowW=234、vRight=1324（视口内），系统信息全部可见。

### v1.35.3（2026-10-26，管理面板体验整改）

- **清除示例数据中的个人标识**：`-demo` 演示数据的服务探测目标原指向真实站点
  （个人域名、博客名、真实 IP），全部换为 RFC 5737 文档保留地址与 example.com 系；
  服务表单占位符、测试用户名同步中性化
- **全部列表统一分页（每页 10 条）**：概览节点状态、全部节点、服务探测、
  节点对比、升级批次明细、负载表、公开备注表、维护窗口、人工事件、审计日志
  新增分页；告警规则 / 资产台账 / 通知队列 / 节点管理 / 告警记录的原 12/15/20
  条每页统一为 10 —— 共 16 处
- **列描述清晰化**：节点表（状态/CPU/内存/磁盘/负载/流量）、服务探测
  （延迟/丢包抖动/证书/可用率）、资产台账（剩余/续费/月摊/剩余价值）、
  通知队列（尝试）、节点管理（Agent）等易混淆表头增加悬停说明
- **详情页布局加固**：`.detail-grid` 主列改 `minmax(0, 1fr)` —— 1fr 的默认
  `min-width: auto` 会让长内容（长主机名/URL/备注）把主列撑到内容宽度、
  挤出侧栏，是「详情页内容溢出」的典型根因；配合 info-v/table-wrap 既有的
  省略与横向滚动构成完整防护

### v1.35.2（2026-10-26，安全审计修复 + 工具链升级）

全栈安全审计（见 `docs/安全审计报告-20261026.md`）的四项修复，每项带回归测试：

- **工具链升级 go1.24.0 → go1.27.3（M1）**：1.24 已 EOL，其标准库有 48 个
  被本项目实际调用的已知漏洞（请求走私、Cookie/查询参数内存耗尽、HTTP/2 DoS
  等公开面真实可达）。升级后 **govulncheck 归零**。`go.mod`、CI workflow、
  构建文档的锚点已同步
- **panelLink 主机白名单（M2a）**：`url.Parse` 放行 Host 里的 `;` `$ ()`
  （实测 `https://a.com$(id)` 可通过），该地址会拼进「节点接入命令」由管理员
  复制到服务器执行。现在主机只接受 `[A-Za-z0-9.:\-\[\]]`
- **接入命令参数单引号包裹（M2b）**：`agentInstallCommand` 的三个插值参数
  一律 `'\''` 转义包裹，拼接侧不信任任何上游 —— 单引号内 `$()`、`;`、空格
  全部成为字面量，与白名单互为双保险
- **公开页令牌失败限速（L1）**：`?t=` 校验失败走独立计数桶（每来源每分钟
  30 次，超限封禁到窗口结束），阻断弱令牌在线枚举；携带正确令牌的正常
  轮询不经任何计数。保存侧本就有 16–64 位字符白名单校验
- **公开页 Cookie Secure 修正（L2）**：反代终止 TLS 时 `r.TLS` 恒为 nil，
  Cookie 的 Secure 改由可信代理的 `X-Forwarded-Proto` 判定（与会话 Cookie 一致）

### v1.35.1（2026-10-26，公开页语义色修复 + 生产切 Caddy/ACME）

- **修复公开页进度条全部透明**：v1.28.0 公开页用 EP 垫片替代 Element Plus 全量 CSS
  （省 361KB），但主题 JS 只注入 `--el-color-primary` 家族，
  **`success/warning/danger/info` 四个语义色从此无人定义** —— 卡片里 CPU/内存/磁盘
  进度条、可用率状态点、网速迷你条、KPI 配色全部解析为透明：数字正常、条不可见，
  且 CSP/缓存等常规排查都指向"正常"。现在 `status.css` 的 `:root` 补齐 EP 官方色值
  （`#67c23a/#e6a23c/#f56c6c/#909399`，深色继承）。教训：**裁掉某个样式来源时，
  要 grep 它原来提供的全部变量，不能只补"正在用"的那一族**
- **生产部署切换到 Caddy + ACME**（`https://status.example.com`）：Let's Encrypt 自动
  签发续期，Caddy 反代 `127.0.0.1:19080`；服务端回绑回环明文、公网 19080 关闭；
  13 台 Agent 删除 `serverCertPin`/`tlsSkipVerify` 走系统 CA **真实验证**，
  「加密但未验证服务端身份」缺口关闭，证书轮换与 Agent 解耦。运维细节见
  `docs/operations.md` 7.6

### v1.35.0（2026-10-26，修完最后三条已知缺陷 + 温度采集）

对 v1.32.0 审计记下的三条已知缺陷全部修掉（每条带回归测试 + 反证），并新增温度采集：

- **A1 网卡指纹**：指纹原来取"流量排序后前 8 块"的明细，流量增量却按**全部**过滤后网卡求和
  —— 超过 8 块网卡的机器上，第 9 块增删改变了求和口径、指纹纹丝不动，
  那次跳变被记成真实流量。现在指纹在截断**之前**、对与求和完全一致的集合计算；
  明细仍截前 8 块（仅供面板展示，控制上报体大小）
  · ⚠ 升级后**首次上报**时，拥有超过 8 块过滤后网卡的节点指纹会变一次，
    该周期会被标 `Incomplete`（提示"请校正本期"）—— 这是口径确实变了，属预期行为
- **A2 规则换指标类别**：节点类（`rule|node` 键）↔ 服务类（`rule|service` 键）之间切换后，
  旧告警的键不再被评估，永远挂着。现在随修改一起关闭（理由"规则指标已修改"），
  并清空该规则的引擎跟踪状态；仅改阈值不动告警
- **A3 探测目标改挂节点**：引擎键是 `rule|service`（不含节点），落库查重却要求节点相同 ——
  改挂后同一问题开出第二条告警，旧的成为僵尸。现在服务告警以**服务**为单位去重，
  改挂节点时原地更新告警的节点字段（与引擎键对齐）
- **新增温度采集**：Agent 读 `/sys/class/hwmon`，上报所有传感器的**最高温**
  （`Metrics.TempC`，无传感器的节点字段整个缺席）。聚合桶取桶内最高值，
  节点详情新增「温度」图表（有数据的节点才显示），新增告警指标 `temp`
  （gt，30–150℃，常用阈值 70–90；无传感器节点不会触发）

**★ 升级顺序：服务端 → Agent → 机群**。`Metrics` 加了 `tempC` 字段，
而 `Report` 用 `DisallowUnknownFields` 解码 —— 先升 Agent 会让旧服务端整份 400。

### v1.34.1（2026-10-26）

- **明文 HTTP 不再回一行让人看不懂的 400**。启用 TLS 后，Go 的标准行为是
  `400 Client sent an HTTP request to an HTTPS server.` —— 在浏览器里就是**一片空白**，
  于是「加了 https 之后页面没数据」这类反馈极难自查。现在同一个端口上先分辨明文/TLS：
  - 明文 `GET/HEAD` → **301 跳转到同端口的 https**（保留路径，片段由浏览器自己保留）
  - 明文 `POST/PUT/...` → 仍回 400：跳过去会丢掉请求体，宁可让客户端看到明确失败
  - TLS 记录首字节固定为 `0x16`，判断不会误伤；HTTP/2 与 SSE 行为不变
  - 日志每分钟最多提示一次「收到明文 HTTP 请求，已跳转到 …」，便于发现别处仍写着 `http://`

### v1.34.0（2026-10-26，通知路由 + 令牌读审计）

对照 Nezha 主控实机数据库（`docs/comparison-nezha-2026-10-26.md`）后补的两处能力缺口：

- **告警规则可以选通知渠道**（`AlertRule.Channels`）：规则编辑弹窗新增「通知渠道」多选，
  留空=全部启用渠道（与旧行为一致）。此前只能全局一刀切 —— 多个渠道时
  「磁盘告警发给谁、登录告警发给谁」无法区分
  · 渠道 ID 打错在保存时就 400，不会等到告警发出那一刻才暴露
  · **渠道被删/停用后回退**：全部不可用 → 改为发送全部启用渠道并在正文说明；
    部分不可用 → 跳过缺失的并列出。告警不会因配置漂移而静默消失
  · ★ 引擎有三条投递路径（`send` / `emit` / `emitMerged`），三处共用同一个
    `ruleChannels()` 解析；只在其中一处接线会让**离线告警静默丢失渠道选择**
    （端到端测试当场抓到过，见 `TestAlertCarriesRuleChannels`）
- **令牌读访问审计**（`api.read`）：以前审计只记写操作，`rollcall-mcp` 这类
  「用令牌调只读 API」的形态完全不可见。现在记录令牌名、来源 IP、方法、路径
  （不含查询串）、状态码与耗时；同一令牌在同一路由上 5 分钟合并一条，
  浏览器会话与匿名轮询不记（否则审计会被面板自己的轮询刷满）
- **令牌台账记录 `lastUsedIp`**：令牌泄露时，「从哪儿被用」比「什么时候」更好定位

### v1.33.2（2026-10-26）

- 设置页的时区显示补上偏移：`time.Local.String()` 在 `TZ` 未设置时是占位的 `Local`，
  单看它无法判断实际时区。现在显示成 `Local (UTC+0)` / `Asia/Shanghai (UTC+8)`
  —— 生产实测正是 `Local (UTC+0)`，也就是说所有按天/按小时的行为都跑在 UTC 上

### v1.33.1（2026-10-26，时区显式化）

对照 Nezha 实机配置时发现的缺口：它的 `config.yaml` 里有显式 `location: Asia/Shanghai`，
我们则依赖**服务器本地时区**。而生产机（以及绝大多数容器）默认是 **UTC** ——
于是「每天 9 点日报」实际在 17:00 触发，「免打扰 22:00–07:00」实际是次日 06:00–15:00，
面板上完全看不出来。

- 新增启动参数 `-timezone <IANA>`，也可用环境变量 `TZ`；内嵌时区数据库（`time/tzdata`），
  所以 distroless/精简镜像里也能用
- 非法时区**启动即失败**，不静默回落到本地时间 —— 那种"以为改了其实没改"最难发现
- 设置页显示**当前生效时区**，并在它是 UTC 时提示怎么改
- 影响面：日报周报发送时刻、免打扰窗口、流量周期的每月起始日、到期提醒的「当天」

⚠ 切换时区会让流量周期按新时区重算一次（`Start` 变化 → 本期用量从 0 重新累计）。
已经积累用量时，建议在周期刚重置后切，或先「校正本期」再切。

### v1.33.0（2026-10-26，面板对外地址可设置）

域名反代场景缺的最后一块：**「面板对外地址」以前只能靠启动参数 `-panel-link` 设置**，
改一次地址就得改 systemd 单元、重启服务。现在面板里可改、会落盘、重启后仍在：

- 设置页新增「⚙ 面板对外地址（域名反代）」。它决定**告警通知里的链接**与
  **Agent 接入命令里的地址**（这两处都会离开当前浏览器，反代后必须用域名）
- 校验：必须是 `http(s)://主机[:端口]` 的站点根；**明确拒绝子路径**
  （`/rollcall/` 这类需要前端整体支持 basePath，目前没有，拒绝比生成打不开的链接好）
- 启动参数 `-panel-link` 仍然优先（容器里用环境变量写死的场景），面板设置作为默认值
- README 反代章节补齐两步必做项：填对外地址、配 `-trusted-proxies`
  （后者决定真实客户端 IP 与 Cookie 的 `Secure`）

### v1.32.0（2026-10-26，缺陷修复批次）

对存量代码做了一轮「只找真 bug」的审计（告警/通知状态机 + 存储/采集/探测），
修掉 9 处能指出具体错误路径的问题，每处都带回归测试（其中两处做了反证：
去掉修复测试即失败）：

- **手动校正后首个上报把整机累计流量记进本期**：校正时若节点还没上报过，
  基线只能是 0，下一条上报会把 `/proc/net/dev` 的开机累计值当增量 ——
  补录 800GB 的机器会瞬间显示 3.1TB 并触发配额告警。新增
  `TrafficCycle.NeedBaseline`，第一条上报只建基线、不累加
- **计数器归零不再静默**：Agent 所在机器/容器重启、网卡重载导致计数器回退时，
  那段流量确实拿不到；现在标记周期 `Incomplete`（面板提示「请校正本期」）。
  时钟回拨不标记 —— 那段流量并没有丢
- **删节点遗留探测目标**：节点删除后挂在其名下的探测目标继续被定时探测（对外发请求），
  而服务告警因「节点不存在」永远打不开，形成不报警的盲区。现在随节点一并删除
- **从备份导入的节点收不到上报**：`ImportConfig` 整体替换节点表却不清墓碑，
  本进程内删过同 ID 后再导入，上报返回 200 但数据被丢弃（永远 `no_data`）。
  「误删 → 导入自动备份」正是最常走这条路的场景
- **CPU 计数器回退导致下溢**：`steal`/`iowait` 的守卫写成了
  `tot > c.prevSteal`（拿累计总时间比上一轮的 steal），恒为真等于没防护；
  iowait 回退时无符号相减下溢成 1.8e19，CPU 曲线瞬间归零、iowait 被 clamp 成 100。
  现在三个计数器各比自己上一轮的值
- **台账级豁免漏掉服务类规则**：勾了「告警豁免」（准备重启/迁移）仍会被
  `service_down` 叫醒。现在服务循环同样跳过，并关闭已有服务告警，
  理由记为「节点告警豁免」
- **摘要吞掉报告正文**：`digest` 会把「每日运行报告 / 到期提醒」也合并成一行标题，
  正文（在线台数、流量、到期日期）永久丢失且任务被标记为已送达。
  现在只有告警（`RuleID` 非空）参与摘要
- **停用规则压掉「已恢复」**：停用规则时把该规则所有待投递通知标为静默，
  包括刚入队的恢复通知；界面告警被关掉、通知里却只有先前那条告警
- **恢复后仍会收到告警**：开启免打扰/摘要或首次投递失败退避时，
  「已恢复」立即送达而队列里的「告警」稍后才到。现在恢复时撤销同一条告警
  未投递的 firing 通知（恢复通知本身保留）

已知未修（下一轮）：网卡指纹取自「按流量排序后前 8 块」而求和覆盖全部网卡
（统计到 >8 块网卡时指纹可能误判，需新增上报字段）；规则在「节点类 ↔ 服务类」
之间切换 metric 后旧告警残留；探测目标改挂节点时可能重复开一条告警。

### v1.31.1（2026-10-26）

- 批量导入生成的接入命令改为**优先使用「面板对外地址」**（设置里的 panelLink），
  没有再回落到请求 Host。此前从回环地址调 API（curl / 自动化）会把命令拼成
  `https://127.0.0.1:19080`——贴到别的机器上是连不上的。浏览器操作不受影响，
  但「命令对不对」不该取决于你是从哪儿点的

### v1.31.0（2026-10-26，批量导入节点）

从其它探针（Nezha 等）迁移时，最耗时的不是装 Agent，而是「逐台点新建 → 逐台点接入
→ 复制命令」。这一版把它压成一次粘贴：

- 节点页新增「⇪ 批量导入」：一行一台，`名称,服务商,地区,国家,分组`（逗号或制表符，
  后四项可留空），一次最多 200 台
- **单行失败不打断整批**：重名（含大小写/空格差异、批内重复）、缺名称、国家码不合法
  都会被跳过并逐行列出原因 —— 迁移数据里重名是常态，整批 400 只会让人逐条试
- 导入完成后弹窗给出**每台自己的接入命令**（含该节点的令牌），可一次性复制；
  弹窗不随列表刷新消失（这批命令正是接下来要用的东西）
- 后端 `POST /api/admin/nodes/import`：令牌一律服务端生成，与单条创建同一路径；
  整批只落盘一次（逐条写会为每台各写一份全量快照）
- 审计动作 `nodes.import`，与其他台账写操作一样留痕

### v1.30.1（2026-10-26）

- 修 v1.30.0 的一个时序问题：HTTP 客户端与 TLS 配置原本构造在
  **首次采集后的一个上报周期之内**，于是「指纹写错」要等 30 秒才报错。
  这不是功能缺陷，但让「启动即失败」的承诺不成立 —— 运维看到的是
  「跑了一会儿才挂」而不是「配置错了」。现在两者都在首次采集前构造
- 验证方式也一并改了：用 `-once` 在 10 秒内跑完一轮，错误指纹会立即
  以 `TLS 配置无效` 退出；正确指纹则打印「已启用服务端证书指纹校验」

### v1.30.0（2026-10-26，Agent 校验服务端证书指纹）

上一版把通道换成 HTTPS，但自签证书 + `tlsSkipVerify` 的实际含义是
**加密但谁都能冒充控制端**：链路上的主动中间人可以用自己的自签证书接管上报，
而上报里有节点令牌。这一版把信任收敛到「安装时那张证书」：

- `agent.json` 新增 `serverCertPin`（叶证书 DER 的 SHA-256，十六进制）。配了它，
  Agent 在 TLS 握手里比对指纹，不符即断开并给出「指纹不匹配」的明确错误
- **指纹优先于 `tlsSkipVerify`**；指纹格式写错时 Agent **启动即失败**，
  不会静默退化成「跳过校验」——那种退化会让用户以为自己在校验身份
- 服务端新增公开接口 `GET /agent/tls-pin` 返回本实例证书指纹；
  `install.sh` 安装时自动取来写进配置，新装节点开箱即带校验
- 面板「节点接入」弹窗展示完整指纹，供安装者人工核对（TOFU 的固有盲区只能靠人看）
- 已装节点升级脚本会提示当前指纹与配置差异，但**不改配置**——
  改动留给操作者显式执行，避免「回滚二进制却带着新配置」的不对等风险

诚实边界：指纹解决的是「证书被换掉」；首次安装那一刻若链路已被控制，
取到的指纹也会被替换。要根治需要证书来自公共 CA（上域名后换 Caddy/Let's Encrypt）。

### v1.29.0（2026-10-26，TLS 切换完成——P0 关闭）

生产从明文 HTTP 对公网切换为**内置 TLS（自签证书，10 年，SAN 含面板 IP/127.0.0.1/localhost）**，
审查报告的 P0 项至此关闭：

- **铺垫（协议）**：Agent `tlsSkipVerify` 配置、install.sh 与升级脚本对 https 目标
  `-k` 下载/健康检查（完整性由 sha256 + 发行验签保证）——服务端与 13 台 Agent 先升
  v1.29.0，全程仍走 HTTP，零风险
- **切换**：自签证书落 `/etc/rollcall/tls/`（属主 rollcall，0600/0640），systemd
  drop-in 追加 `-tls-cert/-tls-key`（保留原 ExecStart 全部参数，删文件即回滚）；
  13 台 Agent 逐台把 server 改为 https 并加 `tlsSkipVerify`（各留 `agent.json.http-bak`）
- **终验**：`https://…:19080/readyz` v1.29.0、`/status` 200、公开 13 节点；明文 HTTP
  不再可用；**13/13 online、版本 v1.29.0**——切换全程无一台掉线
- 浏览器访问会有自签证书警告（无域名的固有代价），接受后即可使用；上域名后可无缝换
  Caddy/Let's Encrypt。诚实声明：跳过校验防窃听不防中间人，下一步增强是 Agent 内置
  公钥校验（TOFU/固定公钥）；Passkey 的浏览器安全上下文已就绪，可排期

### v1.28.0（2026-10-26，公开页 vendor 精简）

状态页默认开启后成为产品门面，但每个访客要拉 Element Plus 全量包（JS 1MB + CSS 361KB
+ dark.css + 语言包）。本轮替换：

- **ep-shim.js 垫片**：以 Vue 函数式组件按**同名接口**实现 status.js 实际用到的
  17 个 Element Plus 组件，`window.ElementPlus` 接口不变，status.js 零改动；Radio 组
  provide/inject 传选中态、Select 退化原生下拉、样式全走 `--app-*` 变量随暗色自适应
- **vendor 删除**：element-plus 四文件移除，dist 从 2.1MB → 608KB；页面核心资源
  gzip 负载 ~75KB（此前 ~400KB+）；CSP/主题包/#jsfail 兜底语义不变
- 验证：demo 浏览器四路由全验（总览/详情/事件/活动/暗色）截图确认正常

### v1.27.0（2026-10-26，搭车批：Agent 协议最后两项 + 机群升级）

- **CPU steal / iowait 采集**：Agent 从 /proc/stat 差分出两列占比进 Metrics；
  详情页「系统信息」在 >0 时显示（steal 高 = 宿主机超售）
- **网卡集合指纹**：Agent 对统计网卡名排序取 SHA-256 经 `X-NIC-Set` 头带来；
  服务端对比变化时**跳过一次增量、重建流量基线并标记周期不完整**——WARP/多 WAN
  切换的大跳变不再被记成真实流量；流量面板警告 +「校正本期」清除。对比/置位/消费
  在同一 AddMetrics 临界区，无崩溃窗口；旧 Agent（无头）不受影响
- **机群升级**：13 台 Agent v1.19.2 → v1.27.0（金丝雀先行，install.sh --upgrade
  从控制端拉取不经 GitHub），面板「可升级」徽标清零

### v1.26.0（2026-10-25，二次走查修复批）

- **向导探测分发启用**：接入弹窗异步探测 `/agent/download.sha256`，服务端未配
  `-agent-dir` 就地显示指引，不再给必败命令；TL;DR 启动命令补 `-agent-dir`
- **TL;DR/方式 0 版本号改 latest-release API 动态获取**（此前钉死，用户第一步就装旧版）
- **agent.exe 真正移出跟踪**（v1.22.1 的移除被拆分提交时的 git reset 吞掉，本次重做）
- **operations.md 补 RC_PREFIX 沙箱用法**；向导手动安装附 unit 模板下载地址

### v1.25.0（2026-10-25，产品默认值：状态页开箱即用）

- **公开状态页默认开启**（v1.15.2–v1.24.x 默认关闭）：探针装好即有 `/status`；
  脱敏保证不变（无 IP/价格/到期日/主机指纹），**显式关闭过的配置升级后保持关闭**；
  总开关/访问令牌/逐台隐藏保留为显式选项。产品方向：默认值以开箱即用为先
- 生产同步清除无人知晓的旧令牌，`/status` 直接可看

### v1.24.0（2026-10-25，告警治理批次）

- **台账级告警豁免**（节点编辑/批量 `alertExempt`）：开启后该节点不再评估任何指标
  规则，既有告警以「节点告警豁免」关闭；三类「不再评估」收尾统一为 closeNodeRule
- **全局离线宽限下限**：设置→告警通知新字段，只抬高不缩短（0=不干预）
- **升级脚本镜像回退**：`--mirror`/`ROLLCALL_MIRROR` 直连失败时回退重试一次

### v1.23.0（2026-10-25，展示层第二批）

- **节点页卡片视图**（表格/卡片切换，移动端默认卡片，批量操作仅表格模式）
- **地区旗帜 + OS 家族徽标 + 卡片年付价徽标**（纯前端映射）
- **概览「正在告警」分页**（10 条/页，页码越界自动收敛，SSE 原位更新兼容）
- **图表口径说明**：非原始区间注明「已按 N 分钟/小时桶聚合」

### v1.22.1（2026-10-25，入门体验批次）

来源：一次终端用户视角的全流程审查（匿名克隆 → 发布包下载 → 源码构建 → 隔离部署 →
Agent 接入 → 面板验证），从零到首台节点上报实测 < 3 分钟。本版修复审查发现的问题：

- **install.sh 沙箱模式**：`RC_PREFIX=/abs/path sh install.sh …` 把二进制/配置/
  systemd 单元样例/日志全部落在前缀下，不创建用户、不碰 systemd——在控制端本机
  （或任何已装 Agent 的机器）演练接入不再有覆盖生产的风险；沙箱进程给出明确清理命令，
  `--upgrade` 与沙箱互斥。生产服务器上用 RC_PREFIX 实机演练验证通过
- **仓库卫生**：误提交的 `agent.exe`（9.3MB）移出跟踪，`.gitignore` 收敛为 `*.exe`——
  匿名克隆从 8.7MB 降到「纯源码」体积
- **README**：顶部新增「最短路径 5 步」（约 3 分钟）；方式 0 补 `chmod +x`
  （GitHub 资产无执行位，先跑 `-version` 会撞 Permission denied）；
  补 Caddy/nginx 反代最小配置——**含 SSE 必需的 `proxy_buffering off`**（nginx 默认
  缓冲会把 `/api/stream` 攒住，面板实时刷新停滞且极难自查）与
  `-trusted-proxies` 配套说明；产物数 12→16 的过期描述修正；
  明确「Agent 仅 Linux 有指标采集」的预期（能力矩阵同步加行）
- **新增 `SECURITY.md`**：漏洞报告走 GitHub Security Advisories，写明设计边界
  （Agent 只读属特性非漏洞），旧版本只合修严重问题
- 测试：install.sh 新增沙箱要素断言与 `sh -n` 语法回归（模板转义错误只有交给
  shell 才会暴露）；23 包全绿

### v1.22.0（2026-10-25）

- **概览「英雄区」两张 KPI**（对标 kokoro/heron 的第一屏聚合统计）：**实时速率**
  （各节点最近两点差分合计 ↓/↑ B/s，在构造 spark 的同一次序列读取里顺带求差，
  零额外历史访问）与**本周期流量**（按各节点计费口径合成的 Used 之和 + 配额百分比）。
  原「累计流量」卡由本周期流量替代（累计值仍在节点详情可见）。`/api/overview` 新增
  `rateIn/rateOut/periodUsed/periodQuota`；`/api/nodes` 每节点新增 `rateIn/rateOut`
- **节点详情新增第 6 张图「磁盘 I/O 速率」**：读+写速率合并曲线（与网络吞吐图同语言），
  头部显示累计读/写。聚合桶只落网络增量、没有磁盘 I/O 增量，因此**只在 1h 原始区间可算**，
  长范围显示明确说明而不是画假曲线（改桶 schema 需升版本，留待真实需求出现）
- **全部 6 张图加「均值 · 峰值」脚注**（对标 heron 的图例）：窗口内均值与峰值一行小字，
  长范围一眼看清常态与极端，不用逐点悬停
- **节点页状态下拉改为可点选 chips**（全部 N · 在线 N · 数据延迟 N · 离线 N · 待接入 N，
  零数量置灰；计数按「服务商/分组过滤后的集合」口径）+ **筛选条件写入 URL**
  （`#/nodes?provider=..&group=..&state=..`，`replaceState` 不产生历史记录），
  刷新/分享可还原
- 验证：23 包测试全绿（速率/周期流量聚合 2 组单测，含 u64 回绕与口径合成断言）；
  demo 浏览器逐项确认（英雄区、1h 磁盘 I/O 图与 7d 提示、6 处脚注、chips 点击后
  URL 同步、刷新还原筛选）

### v1.21.0（2026-10-24）

- **只读 MCP 服务器 `rollcall-mcp`**（`cmd/mcp` + `internal/mcpserver`）：stdio 传输、
  换行分帧 JSON-RPC 2.0，零第三方依赖。7 个只读工具全部映射 `authReadonly` GET 端点，
  鉴权走 `Authorization: Bearer rct_...` 作用域令牌（建议 `scopes=["read"]` + 路径限定），
  与 Agent 单向边界无关——它只是面板的另一个 HTTP 客户端。工具执行失败按 MCP 语义返回
  `isError` 结果（协议层错误才用 JSON-RPC error）；节点 ID 客户端先做路径穿越校验
- 协议兼容：回显客户端请求的 `protocolVersion`（缺省 `2025-06-18`）；通知不应答；
  未知方法 -32601、坏帧 -32700；stdout 只走协议、日志全进 stderr
- 验证：23 个包测试全绿（协议层/工具校验/stdio 循环 5 组单测）；本地 demo 端到端
  ——创建 read 作用域令牌 → MCP 客户端完整会话（initialize / tools/list / 4 个工具调用）
  全部返回真实数据；生产机上用临时令牌跑同一会话，`get_overview` 返回 13/13 在线
- 期间发现并修正过一个**测试脚本的坑**：令牌创建响应里 `meta.prefix`（12 字符）排在
  `token`（52 字符）之前，用 `grep … | head -1` 提取会拿到前缀，面板正确地 401，
  险些误判为产品缺陷——提取令牌必须按完整长度匹配
- Makefile `linux` 目标与 `SHA256SUMS` 增加 `rollcall-mcp-linux-*` 四架构产物

### v1.20.1（2026-10-24）

- **审计查询覆盖整个保留期**。此前内存只保留最近 1 万条，90 天保留期一旦写满，
  更早的记录在面板上永远查不到（文件在、查不到）。现在内存窗口装满且查询范围
  涉及更早记录时，自动回源流式扫描 `audit.jsonl`（文件在保留期内是内存的超集，
  冷路径以文件为准，不与内存重复计数）；扫描在锁外执行，不阻塞审计写入。
  内存与文件两条路径共用同一过滤判定，语义一致；配两组合计 2 万+ 条记录的
  单元测试验证冷路径的命中、分页、倒序与子串搜索
- 顺带核实两条旧建议已是既成事实、从清单划掉：探测并发上限
  （`cmd/server` 调度循环 `sem` 容量 8 + 每探测 8–10s 超时早已存在）；
  顶栏实时/离线徽标（`markSynced`/`markOffline` 已有）

### v1.20.0（2026-10-24）

- **剩余价值估算**：未到期资产按「年付价 × 剩余天数 / 365」折算（USD 口径与月摊一致，
  CNY 按 cnyRate 折算），三处展示：概览 KPI、台账每行 + 合计、对比页分组聚合 + KPI。
  未登记到期日或已过期的资产不计入；成本合计与分组之和保持同口径可对账
  （生产实测 demo：分组 19.75 + 57.7 + 0.81 ≈ 合计 78.27）。当前生产 `remainingValue=0`
  是因为台账价格/到期字段尚未补录——功能就绪，等数据
- **能力矩阵页**（设置页尾部）：静态自述「我们有什么 / 部分具备什么 / 不提供什么」，
  按 KPanel 竞品对照页的 ●◐○ 口径分组呈现，条目链接到对应视图。它是文档不是状态面板，
  与 README 功能清单同步维护
- **Dockerfile**：多阶段构建（golang:1.24-alpine → distroless/static:nonroot），
  自带 CA 证书与 tzdata、无 shell；预建 `/data` 归 nonroot，构建时 `RUN -version` 做冒烟。
  `docker build --build-arg VERSION=v1.20.0 -t rollcall .` 即得与 systemd 部署
  完全一致（`-trimpath -buildvcs=false` 可复现）的二进制
- 生产升级路径：四架构本地构建 → `SHA256SUMS` → Ed25519 签名 → SFTP 上传 →
  `rollcall-server-upgrade.sh --signature`（发行验签 + readyz 版本一致性 + 失败自动回滚）。
  升级日志「发行签名校验通过（版本 v1.20.0）」，`readyz` 返回 v1.20.0，
  13/13 节点在线，线上 sha256 与本地构建/SHA256SUMS 三方一致（`b92f3677…`），
  公开页匿名 404 保持，无错误日志
- `docs/comparison-kpanel-2026-10-22.md` 增补「0.9 更新」节：P0-1/2/3/4、ICMP、
  SSH 登录告警、ServiceSample 结构债等均已落地并标注版本；KPanel 对照页本身停在
  2026-10-04 未更新

### v1.19.3（2026-10-23）

- 节点详情的「登录采集」不再给「正常 / 异常」的判定，只陈述两件事实：
  **wtmp 最后写入时间**与**窗口内最后一条登录记录的时间**，并在超过一天没有登录记录时
  给一句自查提示。原因是生产实测中这两种情况无法与"最近确实没人登录"区分：
  一批节点的 wtmp 已 7–24 天没有写入，另一批只写 `LOGIN_PROCESS`（每 10 秒一条）而没有
  任何 `USER_PROCESS`。给一个可能是错的结论，不如把数字摆出来

### v1.19.2（2026-10-23）

- **修正登录采集状态的误报**。v1.19.1 用「wtmp 最后写入时间」判断可用性，但生产核对发现
  控制端的 wtmp 虽然在写（每 10 秒一条 `LOGIN_PROCESS`），**却没有任何 `USER_PROCESS`
  记录** —— sshd 根本没把 SSH 登录写进去。于是指示器会显示「正常」，而功能实际不触发
- 现在改成按「窗口内是否真有登录记录」判断，并新增 `wtmp-quiet` 状态：
  文件在写但没有登录记录时显示「wtmp 在写，但窗口内没有任何登录记录。若你刚登录过，
  说明本机 sshd 没把登录写进 wtmp」。这两种情况在数据上无法区分，所以只陈述事实
- 读取窗口从 8 条放大到 256 条（96KB）：wtmp 里混着大量非登录记录，窗口太小会让真正的
  `USER_PROCESS` 被挤出视野 —— 生产上就是因此在控制端一条登录都没看到

### v1.19.1（2026-10-23）

- **登录采集的可用性会显示出来**。上线 v1.19.0 后在生产核对发现：13 台里有 12 台的
  `/var/log/wtmp` **根本没在写**（这些镜像缺 `/run/utmp`，`pam.d` 也没配 utmp），
  也就是说登录告警在它们身上永远不会触发 —— 一个静默失效的功能比没有这个功能更糟。
  Agent 现在用请求头（`X-Login-State` / `X-Login-Age`，旧服务端会忽略）上报采集状态，
  节点详情显示「登录采集：正常 / 未在写入（最后写入 N 天前）/ 不可用」
- 判定用 wtmp 的最后写入时间而不是"有没有新登录"：正常节点每次登录与开关机都会写一条，
  一周没写基本可以断定写不进去
- 想真正启用，需要在节点上补 `/run/utmp`（`touch` + `chown root:utmp` + `chmod 0664`，
  并加 tmpfiles 条目）或在 PAM 里配 utmp —— 这属于节点侧运维动作，本项目不做自动改动

### v1.19.0（2026-10-23）

- **节点 SSH 登录告警**：Agent 读 `/var/log/wtmp`，把「自上次上报以来的新登录」
  （用户、来源、终端）随上报带回；服务端去重、落到节点台账（`lastLogin`）并推送通知。
  `wtmp` 只记录**成功**登录，所以这是"确实有人进来了"，失败尝试不在范围
- 只看 `USER_PROCESS` 记录（启动/登出记录会被过滤，否则一次重启就会被报成登录）；
  老登录没有 `ut_host` 时用 `ut_addr_v6` 兜底出 IPv4
- 水位用**时间戳**而不是文件偏移：wtmp 会被 logrotate 轮转，偏移在轮转后指向完全不同的位置
- 首次上报**只建立水位、不报历史**：Agent 装好或重启时 wtmp 里可能躺着几个月前的登录，
  整批当"新登录"推给运维是纯噪音。代价是重启前几秒的登录可能漏掉
- 通知：root 从网络登录抬到 `warn`，其余 `info`；每条登录单独一条（这是安全信号，不适合合并）
- 节点详情新增「最近登录」一行（用户 @ 来源 · 终端 · 多久前）
- 字段全部来自被登录者控制的 wtmp，按不可信输入处理：`sanitize` + 长度截断；
  未来时间戳直接丢弃（否则一条 +10 年的登录会把后续真实登录永久压成"旧事件"、静默掉告警）

**★ 升级顺序：服务端 → Agent → 机群**。`Report` 用 `DisallowUnknownFields` 解码，
先把 Agent 升上去会让整份上报 400、节点直接失联 —— **与本项目上一次协议变更（上报签名）相反**。

### v1.18.0（2026-10-23）

- **探测历史改用独立类型 `ServiceSample`**（`ts` / `lat` / `ok` / `cert`）。
  此前它复用 `Metrics` 的字段：`CPUPct` 存延迟、`TCPConn` 存成功、`Procs` 存证书天数，
  每读一处都要靠注释解释，任何人按字段名理解都会静默得到错误的延迟数据
- **旧快照仍能读**：`ServiceSample.UnmarshalJSON` 同时接受旧键（`cpuPct`/`tcpConn`/`procs`），
  按「键是否存在」判形态而不是看值 —— 0 延迟的旧样本不能被误判成空样本
- **快照 schema 提到 5**：新程序读得懂 v4，但旧程序读不懂 v5。不提升版本号的话，
  降级回 v1.17.x 会把探测历史静默读成「延迟 0、全部失败」，看起来像所有站点同时挂掉；
  提升后旧程序会明确拒绝并备份
- `Ring` 泛型化（`Ring[T]`），节点序列与探测序列共用同一套环形缓冲实现，
  时间戳取值由构造函数注入而不是让 model 类型反向依赖 store

生产验证（v1.18.0，2026-10-23）—— 建一个真实 ICMP 目标、跑满两个探测周期后读回：

| 项 | 结果 |
|---|---|
| 落盘格式 | `schemaVersion=5`，样本 `{"ts":1792760873,"lat":2,"ok":true,"cert":-1}` |
| API 不变 | `latencyMs=2`、`spark=2 点`、`lossPct=0`、`uptime=100`、`certDays=-1` |
| 删除后 | `svcRes` 键数回到 0（v1.17.2 的孤儿守卫仍生效） |
| 一致性 | 本地构建 / 发布 digest / 线上 sha 均为 `2274c9ee…`；四架构在生产机验签通过 |

### v1.17.2（2026-10-23）

- **删掉探测目标后不再留孤儿序列**：删服务的那一刻若有探测在飞，它的结果会给已删服务
  重建一个环 —— 这份数据既不显示、也不会自己过期，只会跟着快照一直存下去
  （生产 `state.json` 里确实看到过一个孤儿的 `svcRes` 键）。现在 `AddServiceResult`
  丢弃不属于现存服务的结果，加载快照时也按现存服务过滤一次（清掉修好之前落盘的脏数据）

### v1.17.1（2026-10-23）

- 修 ICMP 目标解析：**裸 IPv6 字面量被误判成"填了端口"**。
  原实现"含冒号即当端口"，而 `2606:4700::1111` 本身含冒号 —— 生产验证时正是这么被拒的。
  改为先用 `net.ParseIP` 认字面量，再看冒号是不是端口。同时修掉其反面：
  旧判断会把 `example.com:80` 当作 IPv6 放行，让"误填端口"一路走到探测阶段才失败

### v1.17.0（2026-10-23）

- **新增 ICMP（ping）探测目标**：测「机器是否在线 + 往返延迟」，与 HTTP/TCP「服务是否活着」
  互补。走非特权 datagram socket（`SOCK_DGRAM` + `IPPROTO_ICMP`），不需要 root 或
  `CAP_NET_RAW`；只在 `ping_group_range` 不覆盖服务用户时才退回 raw socket，两条路都失败
  会在探测详情里给出可执行的修复建议
  · 沿用同一套 SSRF 策略：ICMP 自己造包、不走 `DialContext`，因此解析出的每个 IP
    都显式过 `CheckIP`，否则「http 被拦的内网地址换成 icmp 就绕过去了」
  · 双栈目标里跳过 IPv6（ICMPv6 的校验和路径不同，未实机验证的不写）并在详情里说明；
    亚毫秒延迟记为 1ms 而不是 0（0 与「未测到」在图表上同形）
- 面板服务对话框新增 ICMP 类型；`type` 白名单与同目标对比接口同步支持

生产验证（v1.17.0 / v1.17.1，2026-10-23）—— 在控制端发起真实 ping：

| 用例 | 结果 |
|---|---|
| `1.1.1.1` | `ok=true`、`latency=1ms`、`echo reply from 1.1.1.1`（**未加任何 capability**） |
| 定时探测写历史 | 间隔 10 秒跑一轮：`latency=1ms`、`spark=2 点`、`loss=0`、`uptime=100` |
| 误填端口 `example.com:80` | 400（v1.17.0 曾误放行，v1.17.1 修正） |
| 带协议 / 带路径 | 400，各自给出对应提示 |
| 裸 IPv6 `2606:4700:4700::1111` | v1.17.0 误拒（"不要带端口"）→ v1.17.1 接受，探测明确回「只支持 IPv4 目标」 |
| 私网 `10.0.0.1` | 控制端带 `-allow-private-probes` 故放行；默认策略由单测确认拒绝 |

### v1.16.0（2026-10-23）

补对照清单里的 P0-2：**上报签名**。此前上报只靠一个静态 `X-Node-Token`，
抓到一次请求就能无限重放，或用自造 JSON 冒充该节点把告警推到 Telegram/企微。

- **协议**：`X-Node-Ts` + `X-Node-Nonce` + `X-Node-Sig`，
  `sig = HMAC-SHA256(token, "v1\nts\nnonce\nsha256(body)")`。
  签名算法放在 `internal/agentwire`，两侧同一份实现，并有固定向量测试钉住格式
- **时间窗 ±90 秒**：超窗直接 401 并提示"请检查本机时钟"；窗口内的立即重放由
  nonce 去重挡住
- **不强制是默认**：升级服务端不会让还在跑旧 Agent 的机器失联。确认面板上
  全部显示"已签名"之后，再用「设置 → 安全与自动化 → 要求签名上报」或
  `PUT /api/admin/report-security` 打开；开启前界面会列出会失联的节点名并要求确认
- **可观测**：节点详情显示「上报签名 / 时钟偏差 ±Ns」（偏差 >30 秒标黄）；
  `GET /api/admin/report-security` 返回 `signedCount` 与 `unsignedNodes`
- Agent 被拒时会把服务端给的原因带进日志（`上报被拒绝: HTTP 401 签名时间戳超出…`），
  不再只有一个状态码

升级顺序（重要）：**先升 Agent，再升服务端，最后才开强制**。新 Agent 发给旧服务端时
这三个头会被忽略，因此先升 Agent 是安全的；反过来先开强制会让旧 Agent 立刻 401。

生产验证（v1.16.0，2026-10-23）—— 用一次性节点发真实 HMAC 请求逐项跑过：

| 用例 | 结果 |
|---|---|
| 升级 | v1.15.3 → v1.16.0 走强制验签；`readyz` = `v1.16.0`，sha256 `3ca55f7b…` 三方一致 |
| 未强制时的未签名上报 | 200（向后兼容，旧 Agent 不受影响） |
| 正确签名 | 200；节点 `signed=true`，`clockSkew` 记录 |
| 用错令牌签 | 401 `签名校验失败（令牌不匹配或请求体被改动）` |
| 时间戳 +200s | 401 `签名时间戳超出允许范围（±90 秒），请检查本机时钟` |
| 同 nonce 重放 | 首次 200 / 再次 401 `重复的 nonce…（可能是重放）` |
| 打开强制后未签名 | 401 `已要求签名上报，但…没有发送签名（升级到 v1.16.0+…）` |
| 机群 | 13/13 Agent v1.16.0，13/13 `signed=true`，最大时钟偏差 **2 秒**，**强制已开启** |
| 公开页 | 匿名 `/api/status` 仍 404 |
| 中途故障 | 一台上海节点首次下载被截断（6195231 / 6287544 B），哈希校验如实拒绝并回退到旧版继续上报；改用单次长超时后成功 |

### v1.15.3（2026-10-22）

补齐对照清单里的 P0-3（四个"小运维"能力，全部走现有通知渠道）：

- **IP 变更通知**：上报来源地址变化时推送，带旧值与新值；首段不同（跨 /24）抬到 critical。
  同一节点 10 分钟冷却，避免双线/VPN 出口反复横跳刷屏；首次接入不通知
- **后台登录通知**：登录成功即推送"账号 + 来源 IP + 第二因素"，不含任何凭据；
  管理员一天登录几次，不做节流——异常登录正是最该立刻被看见的
- **多台同时离线合并**：同一轮评估里的离线/恢复各合成一条（`N 台节点失联`），
  跨轮次不合并；单台保持原来的逐条消息；同一规则下保留 RuleID 以便静默生效
- **本周期流量手动校正**：`POST /api/admin/nodes/{id}/traffic-adjust`（`usedGb` / `usedBytes` 二选一），
  按当前计费口径把本期用量改成给定值，后续增量在其上累加，并写审计；
  节点详情的「流量配额」面板加了「校正本期」入口。用于和服务商面板对账

生产验证（v1.15.3，2026-10-22）—— 用"本机 webhook 指向自身 `/healthz`"当接收端，跑完即恢复配置：

| 项目 | 结果 |
|---|---|
| 升级 | v1.15.2 → v1.15.3 走强制验签 |
| 后台登录 | `sent \| 面板登录 \| 账号 admin 从 127.0.0.1 登录（密码）` |
| IP 变更 | `sent \| 节点上报地址变更 \| …从 203.0.113.5 变为 198.51.100.9…` |
| 多台离线合并 | 3 台测试节点同一轮离线 → `sent \| 3 台节点失联 \| verify-merge-a、…-b、…-c 失联（同一轮检测）` |
| 流量校正 | `usedGb:1.5` → 读回 `used=1610612736`（1.5 GB）、口径 sum；详情与列表一致 |
| 恢复合并 | 生产上删除节点不补发恢复（告警随节点消失），该路径由单测 `TestOfflineRecoveryMerged` 覆盖 |
| 清理 | 测试节点全部删除（13 台）、离线规则 duration 复原 180s、通知配置恢复为关闭 |

### v1.15.2（2026-10-22）

- **公开页默认关闭，并支持访问令牌**：`/status`、`/api/status*`、公开页静态资源与 SSE
  统一走同一个门槛（未打开 → 404）。这是补 KPanel 对照里最严重的一条：此前匿名即可
  拿到全部节点名与在线状态。配套：`POST /api/admin/status-page/token` 一键换链
- **逐台隐藏 + 公开备注**：隐藏节点既不进列表，也不能按句柄查详情；
  公开备注独立于内部 `Note`，避免把含价格/账号的备注泄漏到公开页
- 设置页新增「访问门槛」与「逐台可见性」两块；写入侧校验令牌形状与节点 ID，
  未知 ID 直接 400 而不是静默丢弃

生产验证（v1.15.2，2026-10-22）：

| 项目 | 结果 |
|---|---|
| 升级 | v1.15.1 → v1.15.2 走强制验签；升级后 `/status`、`/api/status`、`/status.css` **全部 404**（默认关闭） |
| 令牌门槛 | 无令牌 404 / 错误令牌 404 / `?t=` 正确 200 / `X-Status-Token` 200 / 带令牌打开页面 200 |
| Cookie 续用 | 首个 `?t=` 请求种下 HttpOnly Cookie 后，`/status.js` 直接 200（页面内资源不必重复带令牌） |
| 逐台隐藏 | 隐藏一台后 `/api/status` 节点数 13 → 12，且该节点的详情句柄返回 404 |
| 公开备注 | 写入 `publicNotes` 后公开列表带出 `note` 字段；内部 `Note`（含价格）不出现在公开页 |
| 写入校验 | 3 位令牌 → 400；未知节点 ID → 400 |
| **发现并修复** | `{"publicNotes":{}}` 清不掉旧备注：`json.Unmarshal` 解到已存在的 map 是**合并**而非替换。改为先读请求体、按「键是否出现」清空；补回归测试（去掉判断即报「空对象应清空备注，实际 旧备注」） |

### v1.15.1（2026-10-22）

- 节点详情页补一行「上报来源」，把 v1.15.0 的 `lastIP` 显示出来（此前只有 API 能看）

### v1.15.0（2026-10-22）

- **节点台账新增「最近上报来源 IP」**（`lastIP`，管理端可见）：上报通过鉴权后由服务端写入，
  台账 API 不能修改；只在 IP 变化时落盘（13 台 30 秒一报，每次保存都会白唤醒快照队列）。
  公开状态页用独立脱敏视图、CSV 导出按列白名单，两处都不会带出这个字段（有测试守着）。
  用途：排查"这台机器从哪个地址连过来"，也是批量运维时定位主机的线索。
- **升级脚本的 `/readyz` 版本核对改为严格相等**：原来用 `grep -Fq '"version":"vX.Y.Z"'`
  做子串匹配，现在用 `sed` 取出 `version` 字段再比对（不依赖 jq），
  多字段顺序、前缀相同（`v1.15.00`）、同名字段（`serverVersion`）都不会误判

生产验证（2026-10-22）：

| 项目 | 结果 |
|---|---|
| 升级 | v1.14.1 → v1.15.0，**走了发行签名强制校验**：`发行签名校验通过（版本 v1.15.0）`；`/readyz` = `v1.15.0`、sha256 `de254195…`（Release 资产/本地构建/线上三方一致） |
| 上报来源 IP | 13/13 节点在重启后首次上报即记录；`/api/status` 与 `assets.csv` 均不含该字段（有测试 + 实测） |
| readyz 严格核对 | 四种输入实测：字段顺序无关 → 命中；`v1.15.00` → 不命中；`serverVersion` → 不命中；精确相等 → 命中 |
| Agent 机群 | 13/13 由 v1.13.1（其中 1 台 v1.14.1）升到 **v1.15.0**，全部 `active`，面板 `agentVersion` 13/13 一致 |
| 分发目录 | `/opt/rollcall/agent` 四架构同步到 v1.15.0，`/agent/download.sha256` 与发布包逐一致 |
| 传输瓶颈 | 实测节点→控制端下载仅 **6 KB/s**（6.3 MB 需 17 分钟），故机群升级改走「节点直连 GitHub + 断点续传 + 校验哈希」，12 台并行 4 并发 8 分钟完成；控制端自身用本地分发文件 |

### v1.14.1（2026-10-22，运维补丁）

- **升级脚本的替换动作恢复原子性**：暂存文件原在 `${TMPDIR:-/tmp}`，而生产机 `/tmp` 是 tmpfs、
  与 `/opt` 不同文件系统 —— 跨 fs 的 `mv` 实为「拷贝 + 删除」，中途崩溃会在服务路径留下
  截断的二进制。改为 `mktemp "$(dirname "$BIN")/..."`，`mv` 即 rename(2)。
  在生产机用真实 systemd 单元 + 19082 隔离实例跑通 upgrade/rollback，
  且 `TMPDIR=/nonexistent` 时仍成功（证明不再依赖 /tmp）；随后生产正式从 v1.14.0 升到 v1.14.1
- `docs/operations.md` 补「自动加密备份调度」与「管理操作审计」两节；
  修正 `expiresMinutes` 最小值的过期描述（实际 1 分钟，界面预置 30 分钟）
- **Agent 分发目录同步到 v1.14.1**：四架构按发布包 `SHA256SUMS` 逐个校验后安装，
  `/agent/download.sha256?arch=` 与发布包一致（此前停在 v1.13.1，新节点装完即显示"可升级"）
- 清理：重复的自动备份 1 份、`/root` 下 98 MB 被 `.prev` 与发布页覆盖的历史构建

### v1.14.0（2026-10-22）

本轮把对比报告里剩下的三条 P1/P2 建议一次做完：公开页目录主题包、探测跨节点并排对比、
API 令牌路径限定与分钟级有效期；另落地服务端发行签名、自动加密备份、审计日志、
容量可观测性与面板行级 patch。控制端已从 v1.13.1 升级到 v1.14.0 并在生产逐项验证：

| 项目 | 结果 |
|---|---|
| 构建与测试 | 用 **CI 同版本 Go 1.24.0**（`GOTOOLCHAIN=go1.24.0`）跑 `go test ./...`：20 包全绿 |
| CI 修复 | 此前 CI 连续 4 次红在 `TestAPITokenPathScopeAndShortExpiry`：它写死 ServeMux 的重定向码为 308，而该码随 Go 版本变化（**Go 1.24 为 301、Go 1.26 为 308**），于是本地 Go 1.26.2 绿、CI 的 1.24.0 红。改为只断言「是 3xx 重定向」，安全性质仍由「清理后的 n10 必须 401」覆盖 |
| 控制端升级 | `deploy/rollcall-server-upgrade.sh` 从 v1.13.1 升到 v1.14.0：sha256 `8f0788be…`、`/readyz` = `{"ready":true,"version":"v1.14.0"}`、服务 active + enabled、`.prev` 保留 v1.13.1、数据目录已打包为 `/root/rollcall-data-before-v1.14.tar.gz` |
| 主题包往返 | 在 `/opt/rollcall/data/themes/verify-pack/` 放入临时主题 → `GET /api/admin/theme-packs` 识别（含 title/version）→ 启用后 `/status` 引用 `/themes/verify-pack/status.css` → 该资源 200 + `text/css` + `no-cache` → `/themes/verify-pack/../etc/passwd/status.css` 返回 404 → 切回内置后页面不再引用 → 删除目录后 packs 为空 |
| 探测并排对比 | 临时建一条 `https://www.baidu.com/` 探测，`GET /api/services/compare` 返回 `vantage=control-server` 并注明「探测均由控制服务器发起」，随后删除该探测（生产探测数回到 0） |
| 令牌路径限定 | 建 10 分钟有效期、仅限 `/api/overview` 的只读令牌：白名单内 200，`/api/nodes` 与 `/api/assets` 均 401，不带令牌 401；用完立即删除 |
| 审计日志 | `data/audit.jsonl` 记录了上述 `tokens.create` / `tokens.delete`（含 actor / role / ip / status），`GET /api/admin/audit` 的 `stats` 显示保留 90 天 |
| 自动备份 | 启用 `-backup-password-file`（密码文件 0400、仅服务用户可读），立即备份产出 5459 字节 `.rcbk`，内置写后解密校验通过（`lastError` 为空） |
| 恢复演练 | 在隔离实例（`127.0.0.1:19081` + 临时数据目录）上用该备份恢复：`nodes=13 / rules=7 / services=0`，节点名称正确；同一文件用错误密码恢复返回 400「备份密码错误或文件已损坏」，证明备份确实受密码保护。演练后临时实例与目录已删除 |
| 备份调度缺陷 | 演练时发现重复备份：启动后首次运行排在 30 秒后，期间手动点「立即备份」仍会按原 deadline 补跑一次。已修（到点先比对 `next`）并补回归测试（去掉判断即报「实际文件数 2」） |
| 可观测接口 | `/api/overview/trend?range=24h` 返回 288 个点、`/api/admin/storage` 返回数据目录/审计/备份容量与保留策略、`/api/admin/health`、`/api/admin/agent-upgrades` 均 200 |
| 公开页重写 | `/status` 走 Element Plus 资源（`/vendor/element-plus/index.css` 200、361 KB、`no-cache`），页面仍为 `default-src 'self'` 的 CSP；`/api/status` 13 节点 |
| 未做 | 发行公钥未在控制端注册（本次升级按 `--allow-unsigned` 显式跳过验签）；Agent 自 v1.13.1 起无源码变更，机群仍为 v1.13.1（面板可用 `/agent/install.sh --upgrade` 或 Agent 升级清单逐台推进） |

### v1.13.1（2026-10-21）

已在 Debian 13 / AMD EPYC 16 核 / 32GB 环境（Go 1.27.3）完成控制端 + 12 台远端被控端的验证与升级（v1.13.1）：

| 项目 | 结果 |
|---|---|
| 构建与测试 | 生产机 `gofmt -l` 干净、`go vet ./...`（linux 与 windows 目标）、`go test ./...`（20 包，其中 18 个含测试）、`CGO_ENABLED=1 go test -race ./...` 全部通过 |
| 发布产物完整性 | `make linux VERSION=v1.13.1` 产出 12 个二进制，`SHA256SUMS` 恰好 12 行且含 4 个 `rollsign-linux-*`，`sha256sum -c` 全过 |
| CI 发布 | tag `v1.13.1` 触发流水线：test / race / release 三个 job 全绿，Release 含 12 个二进制 + `SHA256SUMS`；发布包 `rollcall-server-linux-amd64` 的 sha256 为 `9d5a391f…`，`rollcall-agent-linux-amd64` 为 `641a1f2f…` |
| 控制端升级 | 生产机用 `deploy/rollcall-server-upgrade.sh` 从 v1.13.0 升到 v1.13.1（sha256 取自 Release 的 `SHA256SUMS`）：`.prev` 保留、`/readyz` 返回 `{"ready":true,"version":"v1.13.1"}`、服务 active + enabled；升级前数据目录已打包为 `/root/rollcall-data-before-v1.13.1.tar.gz` |
| Agent 分发目录 | `/opt/rollcall/agent` 四个架构全部换成 v1.13.1，`/agent/download.sha256?arch=` 返回的哈希与 Release 中对应文件一致，新接入的节点不再一装就落后 |
| 机群升级 | 13/13 节点（1 台本机 + 12 台远端）由 v1.10.4 升到 v1.13.1，面板 `agentOutdated` 为 0；升级后 13 台 online、0 stale / 0 offline / 0 noData |
| 升级异常与处置 | 一台上海节点 到控制端的下载被链路反复截断（`curl: (18) end with N bytes missing`，两次分别停在 ~600KB），改为把 Release 二进制投递到节点、校验 sha256 后按脚本同样的步骤替换（保留属主、备份 `.prev`、systemd 重启失败即回滚），节点最终生效版本仍为 Release 同一哈希 |

> **v1.13.1 的 Agent 侧确有变更**（NFS/CIFS/sshfs 挂载点不再调用不可中断的 `statfs(2)`，
> 避免网络存储失联时采集协程整体挂死；另含 `readMounts` 设备去重顺序、Hyper-V 误判与日志脱敏修复），
> 因此本次把 12 台远端节点与分发目录一并升级，而不是继续保留旧版本。
> 升级后各节点保留 `/usr/local/bin/rollcall-agent.prev` 作为单机回滚点。

以下为 v1.10.5 及更早版本的验证记录：

| 项目 | 结果 |
|---|---|
| 构建与测试 | `gofmt -l` 干净、`go vet ./...`、`go test ./...`（18/18 包）、`CGO_ENABLED=1 go test -race ./...`（18/18 包）全部通过 |
| 模块路径 | 模块名与仓库地址对齐为 `github.com/motao123/rollcall`（81 个文件），旧路径在源码与部署模板中已无残留；`go install …/cmd/server@latest` 与 `/agent/install.sh` 里的 `Documentation=` 从此指向真实仓库 |
| 发布产物完整性 | `make linux VERSION=v1.10.5` 产出 12 个二进制，`SHA256SUMS` 恰好 12 行且含 4 个 `rollsign-linux-*`（此前 `rollcall-*` 通配会漏掉签发工具本身），`sha256sum -c` 全过 |
| 升级脚本路径解析 | 隔离实例上只传 `--service` 时，二进制路径由单元 `ExecStart` 推导（`/opt/rcwalk/rollcall-server`），v1.10.4 → v1.10.5 升级、`.prev` 备份、`/readyz` 版本核对、回滚 v1.10.5 → v1.10.4 全部成功 |
| 升级脚本拒绝路径 | `--bin` 与 `--service` 的 ExecStart 不一致 → 退出 1 且不触碰任何文件（生产二进制哈希前后一致）；单元不存在 → upgrade/rollback 均退出 1；缺 curl/wget 且未显式 `--health ""` → 退出 1；`--health ""` 正常跳过检查；`--help` 只输出注释头，不再漏出 `set -eu` |
| 会话用户对象 | `/api/auth/login` 与 `/api/auth/session` 的 `user.totpEnabled` 是 JSON 布尔（曾为字符串 `"false"`，会让集成方的 `if (user.totpEnabled)` 恒为真）；`user` 只含 `username`/`email`/`totpEnabled`，无 `passwordHash`/`totpSecret`/`updatedAt`/`lastTotpStep` |
| Agent 单元一致性 | 新增回归测试逐项比对 `/agent/install.sh` 生成的单元与 `deploy/rollcall-agent.service` 的 14 条沙箱指令；人为删掉任一侧的 `RestrictSUIDSGID` / `MemoryDenyWriteExecute` 均能让测试失败（双向验证过），两处 `Documentation=` 一致 |
| 生产升级 | 用 `deploy/rollcall-server-upgrade.sh` 将控制端 v1.10.4 → v1.10.5：sha256 取自 `dist/SHA256SUMS`，升级后哈希 `7a9d0000…`、`/readyz` 返回 `{"ready":true,"version":"v1.10.5"}`、`/healthz` ok、服务 active + enabled、`.prev` 保留 v1.10.4 作为回滚点 |
| 升级后回归 | 13/13 节点在线，0 stale / 0 offline / 0 noData；`admin.token`、`admin-user.json`、`session.key` 均 0600；无 `initial-admin-password.txt` 残留；`/app.js` gzip + `Vary` + 弱 ETag，`If-None-Match` 命中 304；`/agent/install.sh` 从不压缩；未登录 `/api/admin/nodes` 401；`/agent/download.sha256?arch=amd64` 与分发文件、下载内容三者哈希一致（`dcf60967…`）；公开 `/api/status` 不含主机名/IP/价格 |
| CI 发布任务 | `.github/workflows/ci.yml` YAML 解析通过；新增 `release` job 仅在 tag 上运行、`needs: [test, race]`、`permissions: contents: write`、钉死 Go 1.27.3、校验 `SHA256SUMS` 为 12 行后用 `gh release create --verify-tag` 发布 |

> v1.10.5 **没有 Agent 侧行为变更**（唯一涉及 agent 源码的改动是 import 路径字符串，不进二进制），
> 当时因此没有升级 12 台远端节点与分发目录，避免一轮没有收益的机群升级。
> v1.11 起 Agent 确有变更（见上方 v1.13.1 记录），机群已在 v1.13.1 全部升级到位。

以下为 v1.10.4 及更早版本的验证记录：

| 项目 | 结果 |
|---|---|
| 构建与测试 | 生产机 `gofmt -l` 干净、`go vet ./...`、`go test ./...`、`CGO_ENABLED=1 go test -race ./...` 全部通过；`make linux VERSION=v1.10.4` 产出四架构 server/agent/rollsign 共 12 个二进制，`sha256sum -c SHA256SUMS` 全过 |
| 可复现构建 | 生产机与另一台 Debian 13 主机分别用 Go 1.27.3 构建 agent amd64，sha256 均为 `dcf60967…`，与分发目录一致 |
| 深度健康检查 | `/readyz` 返回 `{"ready":true,"version":"v1.10.4"}`；数据目录不可写、首次快照未落盘等故障场景返回 503；升级脚本以 `/readyz` 版本匹配作为成功判据 |
| 一键升级 | 生产机执行 `rollcall-server-upgrade.sh upgrade`（sha256 校验 + `.prev` + `/readyz` 版本核对）成功；隔离实例验证坏二进制会失败并自动回滚到旧版本 |
| 首次登录 | 环境变量方式启动后无密码文件残留；隔离实例验证 `initial-admin-password.txt` 权限 0600、首次登录成功后被删除；非回环明文启动被拒绝时不落任何凭据文件 |
| 凭据与安全 | 错误密码 401、未登录管理 API 401、写请求缺少 `X-Requested-With` 403；`admin.token`/`session.key` 0600；会话跨服务重启保持 |
| 静态资源 | `/app.js` 返回 gzip + `Vary` + 弱 ETag，`If-None-Match` 命中 304；`/agent/install.sh` 从不压缩；公开 `/status` 与面板登录页正常渲染，无控制台错误、无横向溢出 |
| Agent 分发 | `/agent/download.sha256?arch=amd64` 返回的哈希与下载二进制一致；一键安装脚本在控制端本机完整走通（下载、SHA-256 校验、低权限用户、systemd 单元、启动） |
| 机群恢复 | 12 台远端节点 + 本机控制端全部升级至 v1.10.4；面板与公开页 13/13 在线、0 离线、0 无数据，公开 `/api/status` 仍不含 `agentVersion` |

以下为 v1.10.3 及更早版本的验证记录：

| 项目 | 结果 |
|---|---|
| 服务状态 | rollcall-server / rollcall-agent 均 v1.10.3、active + enabled；13/13 节点在线 |
| 回归测试 | Ubuntu 上 `gofmt -l` 干净、`go vet ./...`、`go test ./...`、`CGO_ENABLED=1 go test -race ./...` 全部通过；staticcheck 与 govulncheck 无发现；gosec 仅剩已逐项复核的演示随机数/插件路径类误报；Node 20 对 `app.js`/`status.js` 语法检查通过 |
| 端到端走查 | 隔离实例手工走通 116 项检查：P0–P3 全部新功能、通知模板、摘要/免打扰、证书/服务/流量告警、DNS/抖动/丢包、TOTP、范围化令牌、加密备份、报告、升级/回滚（当时的检查脚本未随仓库发布，无法一键复现；可执行的清单见 `docs/operations.md` §10） |
| 面板账号 | 设置页可直接修改用户名/邮箱与密码，均需当前密码确认；改后当前浏览器续签、其他设备旧会话立即失效；旧用户名/旧密码登录 401，新凭据可登录；`admin-user.json` 仍仅存 PBKDF2 哈希 |
| 服务端升级脚本 | 生产机用 `deploy/rollcall-server-upgrade.sh`：v1.10.2 → v1.10.3，强制 sha256 校验、`.prev` 保留、健康检查失败自动回滚；升级后 `/healthz` ok、登录 200/未登录 401 |
| Agent 升级脚本 | 生产机执行 `install.sh --upgrade`：v1.10.2 → v1.10.3，配置未改动，`.prev` 保留 v1.10.2；分发目录同步更新 amd64/arm64 二进制 |
| Agent 版本上报 | 本机 Agent 上报 v1.10.3 且不再提示可升级；公开 `/api/status` 不包含 `agentVersion`（远端节点多为旧版 Agent，未发送版本头） |
| 审计修复 | 未认证上报不再计入健康速率（生产连发 3 次无效上报，`last60s` 保持 26 不变）；健康卡片区分 noData；流量配额支持 `lt` 且阈值边界与运算符一致；到期提醒/日报/周报/月报仅在真正入队后写去重标记；到期天数跨夏令时按日历日计算；计数器忽略远期时钟回拨桶 |
| 后台一致性 | 全页面统一输入框/选择框/textarea 规格（12.5px、7px 圆角、聚焦光晕）；服务、告警、资产、对比、状态页模块顺序调整；路由级搜索框只在支持的列表页显示并带专属占位符；服务目标支持编辑、启停与「检测」即时探测（含延迟/状态码/证书天数）；资产续费、状态页维护窗口与事件均可在面板内编辑/结束/删除 |
| 轮询刷新 | 隔离实例实测：顶栏同步时间每约 5 秒推进；聚焦搜索框 22 秒内 CPU 连续 30%→35%→40%→45%，轮询不再被输入焦点冻结；节点详情每约 5 秒比对，上报后 1 个周期内更新图表且不重复请求；勾选节点 28 秒内选择保留、数据照常拉取，取消勾选后 5 秒内补上最新值（20%→40%）；390px 视口隐藏同步时间且无横向溢出；全程无控制台错误 |
| 新告警闭环 | 服务宕机、TLS 证书剩余天数、月度流量配额三条默认规则在生产创建成功（201）；告警触发 Webhook 与恢复通知、到期提醒均进入 Outbox |
| 网络质量 | DNS/HTTPS/TCP 探测返回延迟、抖动与丢包；公开页展示「可用率 · 抖动 · 丢包」，探测全失败时标记「探测失败」 |
| 通知降噪 | 免打扰窗口、摘要聚合、自定义标题/正文模板对 Webhook 同样生效；每日/每周/每月报告可配置并经通知通道送达 |
| 资产与续费 | 台账页续费日历按月列出到期与续费记录；到期提醒按 `expiryDays` 提前发送且同一资产只提醒一次 |
| 对比与收藏 | 对比页展示分组聚合、节点横向对比、收藏优先排序；收藏状态持久化 |
| 公开页增强 | 地区分布、事件时间线与最近活动流正常渲染；公开接口仍不含主机名/IP/价格等敏感字段 |
| 安全增强 | TOTP 登录（RFC 6238，±1 窗口）与 `rct_` 范围化 API 令牌（read/admin、SHA-256 落盘、重启后仍可匹配）；`/metrics`、管理令牌 API 正常 |
| 加密备份 | AES-256-GCM 导出/导入仅覆盖配置（节点、探测、规则、静默、续费、设置与节点令牌），导入需确认且不触碰监控历史 |
| 采集粒度 | 本机节点上报多挂载点、多网卡与磁盘 I/O；`/api/admin/nodes` 详情可见 mounts/nics/diskIo，快照 schema 升为 4 |
| UI 冒烟 | 概览、对比、台账、状态页、设置、公开页均正常渲染；1323px 视口无横向溢出、控制台无错误；登录、会话与退出正常 |
| 会话吊销 | 退出登录后旧 Cookie 访问会话接口返回未登录、管理 API 401；重启控制端后旧 Cookie 仍处于吊销状态（吊销列表已持久化）；仅接受规范 base64 令牌，改写末位字符无法绕过吊销 |
| 升级提示 UI | 概览、节点列表、设置页与详情页均显示「可升级」（临时实例模拟旧 Agent）；636px 宽无横向溢出，浏览器控制台无错误 |
| 公开状态页 | `/status` 匿名可访问；13 个节点实测在线；1440px 三列 / 390px 单列均无横向溢出；浅色、深色、跟随系统切换正常 |
| 公开 API | `/api/status`、`/api/status/node/<hid>`、`/api/theme` 匿名 200；用内部节点 ID 访问详情返回 404；匿名 `/api/nodes` 401；`/metrics` 凭管理令牌 200 |
| 公开页脱敏 | 默认不下发地区、服务商、主机名、系统、内核、架构、CPU 型号、核心数、虚拟化、进程数、TCP、Swap；`/status` 与 `/api/status` 无内部 ID |
| 详情趋势 | 24h / 7d / 30d 切换正常，服务端降采样上限 240 点；空载 `0.00` 负载仍会下发而不是被当作空字段 |
| 详情页布局 | 资源环与当前状态并入头部；趋势图双列铺满；可用率/服务延迟/主机信息宽屏三列、窄屏单列；390 / 640 / 1280 / 1440px 无横向溢出 |
| 增量刷新 | 15 秒轮询保持搜索输入焦点与筛选状态；页面隐藏时暂停；详情切换区间不再整页闪烁 |
| 面板登录 | 用户名与邮箱（大小写不敏感）均可登录；错误密码 401；会话跨刷新与服务重启保持；退出后 Cookie 立即失效 |
| 面板布局 | 640px 移动端概览：区块标题的元信息与按钮右对齐、奇数 KPI 卡片补满整行、告警角标无告警时隐藏且不挤占导航项高度 |
| 侧栏主题按钮 | 桌面端与折叠侧栏下均保持单行水平排列，图标与文字不换行、不被裁切；1080px 以下只保留居中图标 |
| 管理端宽度 | 2000px 宽屏下顶栏与正文同宽（上限 1440px）居中，长短页面切换无滚动条横向跳动；1280px 概览/节点表按列宽横向滚动，390px 下服务、告警、台账、设置、状态页表格均保持单行并由容器横向滚动；空表不出现横向滚动条 |
| 鉴权边界 | 未登录读写接口 401；会话写请求缺 `X-Requested-With` 返回 403；管理令牌 API 与 `/metrics` 仍可用 |
| 凭据存储 | `admin-user.json` 仅存 PBKDF2-SHA256 哈希（600,000 次迭代），无明文密码；`session.key` 0600 |
| 上报频率 | 30 秒一报，在线/数据延迟/离线状态判定正确 |
| 指标正确性 | CPU/内存/磁盘与 `df`/`free` 交叉核对无偏差 |
| 数据持久化 | state.json 与 gzip history.json 重启后保留；生产快照 schema 4、历史 schema 2，旧版本快照可自动载入 |
| 历史范围 | 1h/6h/24h 原始点；7d/30d/365d 走 1m/5m/1h 聚合，全年合成数据 UI 验证通过 |
| 告警与通知 | 默认规则加载；Outbox 指标正常；插件投递、环境隔离与拒绝全局可写目录在独立实例验证 |
| 主题 | 公开读取默认主题，管理端校验、保存与重启后持久化均正常 |
| Agent 自更新 | 独立实例验证签名安装、启动计数、自动/手动回滚与 7 天冷却 |
| 安全约束 | Agent 依赖表无 `os/exec`、监听端口数 = 0；服务端 `net.Listen` 符号 > 0 |
| 响应式界面 | 1440px 桌面端（侧栏 + 顶栏）与 390px 移动端（底部导航）登录、概览、设置页实测通过 |
| 内存占用 | server 14.3 MB / agent 7.9 MB |

完整操作步骤与升级/回滚方法见 `docs/operations.md`；界面截图见 `docs/screenshots/`
（全部取自 `-demo` 演示数据：虚构节点 `demo-*`，不含任何真实部署信息）。
`-demo` 模式即可预览与截图一致的效果：`./rollcall-server -demo -addr 127.0.0.1:19080`。

---

*License: MIT*

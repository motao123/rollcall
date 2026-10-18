# Rollcall — 轻量机群监控平台

> 曾用名 **Probe**（2026-10-18 改名）。`docs/` 下的两份历史报告仍使用旧名，为当时事实记录，保留原貌。

一个**零外部依赖**的自托管服务器监控平台。融合 Nezha 的「服务探测能力」与 Komari 的「极简部署与清爽界面」，并按"Agent 只读"的安全原则重新设计。

```
┌────────────────┐                          ┌─────────────────┐
│ rollcall-agent │                          │ rollcall-server │
│  （只读采集）  │      ──▶ 单向 POST       │（Go + 内嵌前端）│
└────────────────┘                          └─────────────────┘
                                                    │
                                            ┌────────────────┐
                                            │ Web UI（原生） │
                                            │    5 个视图    │
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
go tool nm /tmp/server-full | grep -cE 'net\.Listen|TCPListener' # 期望：>0（实测 9）
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
- 节点详情：五张时序图（CPU / 内存 / 磁盘 / 网络速率 / 负载），支持 1h / 6h / 24h 切换

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

### 4. 资产台账（两者都没有）
这是 40 台规模最现实的痛点：**忘续费 = 直接丢机器**。

- 字段：服务商、地域、分组、到期日、年付价、币种、备注
- 30 天内到期自动进入「即将到期」列表（7 天内标红）
- 月度成本自动折算（年付 ÷ 12，CNY 按汇率折算为 USD 聚合）
- 节点卡片与台账共用同一份数据，不重复录入

### 5. 公开状态页（免登录）

访问 `/status` 得到一个**完全公开、只读**的状态页，可直接挂到域名下：

- 顶部横幅：全部正常 / N 个节点离线、N 个服务异常
- 服务区：名称 + 24h 可用率（配色按可用率方向：≥99.5 绿 / ≥95 琥珀 / 更低红）+ 延迟
- 节点区：按分组展示，每节点含 CPU/内存/磁盘/运行时长 + **48 段 24h 可用性色块**
- 30 秒自动刷新；跟随系统深浅色；移动端单列自适应
- 自包含（内联 CSS + 外部 status.js），零外部资源请求

**脱敏原则**（公开页可被任何人访问）：
不含 IP、服务商、价格、到期日、备注、主机名、内核、架构、节点 ID、上报令牌。
仅保留：展示名、分组、在线状态、三项使用率、运行时长、24h 可用率色块。

**与 Basic Auth 的关系**：启用 `-auth` 后，`/` 管理界面需登录，
但 `/status` 与 `/api/status` **仍可匿名访问**（这是公开页的设计意图）。
管理 API 与 Agent 上报不受影响（前者需令牌，后者需节点 token）。

---

## 三、数据采集与展示

### 采集方式：只读、单向、无指令通道

Agent 每 30 秒读取以下文件，**不做任何其他操作**：

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

### 存储：环形缓冲 + JSON 快照

对齐 Komari 的"无数据库"理念，同时保证重启不丢历史：

- 每节点一个**定长环形缓冲**（2880 点 = 24h @30s），全量常驻内存约 30MB
- 每分钟把状态快照写入 `data/state.json`（先写 `.tmp` 再 rename，保证原子性）
- 启动时自动恢复；快照损坏会隔离为 `state.json.corrupt-<时间>` 并拒绝启动，
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
│          │  顶栏：标题 / 搜索 / 实时指示 / 刷新      │
│  侧边栏   ├────────────────────────────────────────┤
│  导航     │  KPI 行（6 项关键指标）                 │
│  主题     │  正在告警（仅异常时出现）                │
│          │  节点卡片网格                            │
│          │  即将到期表格                            │
└──────────┴────────────────────────────────────────┘
```

五个视图：**概览 / 节点 / 服务 / 告警 / 台账**，左侧导航切换，节点详情走 `#/nodes/{id}`。

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
- 构建机：Go 1.22+、git、curl
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
> 使用前需要构建机安装 `make`。

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

若不需要探测/通知内网目标，请从 unit 的 `ExecStart` 中删除
`-allow-private-probes -allow-private-notify`；这两个参数应仅在明确需要时开启。

> 服务端收到 SIGINT/SIGTERM 后会停止接收新请求、等待在途请求结束并立即保存快照，
> systemd 重启不会丢失最近一次自动保存（60 秒）之后的数据。

### 4. 首次登录

```bash
sudo cat /opt/rollcall/data/admin.token
```

打开 `http://127.0.0.1:19080`，两种方式任选：

- 访问 `http://127.0.0.1:19080/#admin=<令牌>`（令牌在 URL 片段中，不会发送给服务器）；
- 或在「设置」页粘贴令牌并保存。

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
# 1. 安装二进制与配置目录
install -m 0755 rollcall-agent /usr/local/bin/rollcall-agent
install -d -m 0750 /etc/rollcall

# 2. 写入配置（server/nodeId/token 用上一步返回值）
#    server 必须是 https://...；仅本机回环可用 http://127.0.0.1:19080
cat >/etc/rollcall/agent.json <<'EOF'
{
  "server": "https://rollcall.example.com",
  "nodeId": "n_xxxx",
  "token": "<48位上报令牌>",
  "interval": 30
}
EOF
chmod 600 /etc/rollcall/agent.json

# 3. 安装 systemd 单元
install -m 0644 rollcall-agent.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now rollcall-agent

# 4. 验证一次上报（失败会以非 0 退出，约 2 秒）
rollcall-agent -config /etc/rollcall/agent.json -once
```

> Agent 默认拒绝向**非本机**的明文 HTTP 服务端上报（节点令牌会裸奔）。
> 远程节点必须使用 `https://`；仅本机回环或明确接受风险时才加 `-allow-insecure`。
> Agent 不跟随 3xx，确保 `X-Node-Token` 不会被转发到其他主机。

> **端口约定**：全文统一用 **19080** 作为生产端口（与 `deploy/rollcall-server.service` 一致）。
>
> **安全默认（1.2.0 起）**：监听非回环地址时，未启用 Basic Auth 或未配置 TLS 会**直接拒绝启动**。
> 三种对外方式：
> 1. 反向代理到 `127.0.0.1:19080`（推荐，由反代终止 TLS）；
> 2. 内置 TLS：`-addr 0.0.0.0:19080 -tls-cert fullchain.pem -tls-key privkey.pem -auth user:<强密码>`；
> 3. 仅限可信内网：显式加 `-allow-insecure` 接受明文 HTTP（不推荐）。
>
> 如需匿名公开只读面板，必须显式加 `-allow-public`；只加 `-addr 0.0.0.0` 已不会再"静默暴露"。

完整参数：

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-addr` | `127.0.0.1:19080` | 监听地址；非回环需满足上面的安全条件 |
| `-demo` | false | 载入 15 台演示数据，使用临时数据目录，不发起真实探测 |
| `-panel-link` | 空 | 面板对外地址（用于告警链接和 Agent 接入指引） |
| `-tls-cert` / `-tls-key` | 空 | 同时提供则启用 HTTPS |
| `-auth` | 空 | `user:password`，启用 Basic Auth（密码不可为空） |
| `-allow-public` | false | 允许无 Basic Auth 时监听非回环（公开只读） |
| `-allow-insecure` | false | 允许非回环地址使用明文 HTTP |
| `-allow-private-probes` | false | 允许 HTTP/TCP 探测内网与回环目标 |
| `-allow-private-notify` | false | 允许通知通道访问内网/回环目标（自建 Gotify/Bark/Webhook） |
| `-insecure-probe-tls` | false | 探测 HTTPS 时跳过证书校验（自签名目标） |
| `-probe-interval` | `10s` | 探测调度粒度；每个服务按自身 `interval` 执行 |
| `-admin-token` | 自动生成 | 管理令牌，显式指定时至少 16 字符 |
| `-data` | `./data` | 数据目录（state.json / admin.token，均为 0600） |

### 管理 API

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/admin/nodes` | 列出节点（含上报令牌） |
| POST | `/api/admin/nodes` | 创建节点，服务端生成 id 与令牌 |
| GET | `/api/admin/nodes/{id}` | 查看单个节点 |
| DELETE | `/api/admin/nodes/{id}` | 删除节点 |
| GET/POST | `/api/admin/services` | 列出 / 创建探测目标（`enabled` 默认 true） |
| DELETE | `/api/admin/services/{id}` | 删除探测目标 |
| GET/POST | `/api/admin/settings` | 读取（脱敏）/ 保存通知配置 |
| POST | `/api/admin/test-notify` | 发送测试通知，可带 `{"channelId":"..."}` 只测单个 |
| GET/POST | `/api/admin/rules` | 列出 / 创建告警规则 |
| GET/PUT/DELETE | `/api/admin/rules/{id}` | 查看 / 修改 / 删除单条规则 |
| GET | `/api/admin/rule-meta` | 规则表单元数据（指标/级别/时长选项，需令牌） |

公开接口（无需任何凭据）：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/status` | 公开状态页 HTML |
| GET | `/api/status` | 公开状态页数据（严格脱敏） |
| GET | `/healthz` | 健康检查 |

一律要求请求头 `X-Admin-Token`。令牌首次启动时自动生成到 `data/admin.token`（0600），也可用 `-admin-token` 显式指定。

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

**C. 内置 TLS + Basic Auth（无需面板）**

```bash
# 在 rollcall-server.service 的 ExecStart 追加：
-auth admin:<强密码> -addr 0.0.0.0:19080 \
  -tls-cert /etc/rollcall/fullchain.pem -tls-key /etc/rollcall/privkey.pem
```

> 非回环绑定要求同时具备 `-auth` 与 TLS（任一缺失都会拒绝启动）。
> 若确实要用明文 HTTP 暴露到可信内网，必须显式加 `-allow-insecure`；
> 若要匿名公开只读面板，必须显式加 `-allow-public`。

管理令牌与 Basic Auth 失败会按来源 IP 限速（5 分钟内 10 次失败封禁 10 分钟），
并记录 `ip/method/path` 审计日志；`X-Admin-Token` 仅通过请求头传递，
首次引导的令牌改走 URL 片段（`#admin=…`，不会发送给服务器或进入访问日志），
`?admin=` 查询串已不再支持。

针对面板环境的设计取舍：不依赖 Docker（避免与面板容器管理冲突）、文件不侵入 `/www/server` 或 `/opt/1panel`、端口避开 888/7870/8888、仅绑 localhost 天然适配反代。

---

## 六、项目结构

```
rollcall/
├── cmd/
│   ├── server/main.go       服务端入口
│   └── agent/main.go        只读 Agent 入口
├── internal/
│   ├── model/               数据结构（无依赖）
│   ├── store/               环形缓冲 + JSON 快照
│   ├── collect/             /proc 采集（linux / 非 linux 两套）
│   ├── netguard/            出站地址策略（SSRF 防护，探测/通知共用）
│   ├── probe/               HTTP/TCP 探测
│   ├── alert/               告警引擎
│   └── web/
│       ├── web.go           API + 内嵌静态资源
│       └── dist/            index.html / app.js / style.css
└── deploy/                  systemd 单元与配置样例
```

---

## 七、安全设计要点

| 风险 | 本项目的处理 |
|---|---|
| Agent 被用作 RAT | 不 import `os/exec`，无任何命令执行路径 |
| Agent 被远程控制 | 协议单向（仅 POST 上报），服务端不连 Agent |
| Agent 开放端口 | 不 `net.Listen`，无入站攻击面 |
| 上报伪造 | 每节点独立 token，**常量时间比较**；服务端时间戳校正 |
| 请求体攻击 | `MaxBytesReader` 限制 64KB；`DisallowUnknownFields` |
| XSS / 点击劫持 | 前端全量 `esc()` 转义；CSP `default-src 'self'` + `frame-ancestors 'none'`；`X-Frame-Options: DENY` |
| 响应头 | `X-Content-Type-Options`、`Referrer-Policy`、HSTS（反代终止 TLS 时同样生效） |
| 凭证外泄 | `Node.Token` 标记 `json:"-"`；管理令牌只走请求头；首次引导令牌走 URL 片段 |
| 误暴露到公网 | 默认只绑回环；非回环必须显式满足 `-auth`/TLS/`-allow-*`，否则拒绝启动 |
| 暴力破解 | 按 IP 统计 401，5 分钟内 10 次失败封禁 10 分钟；失败请求写审计日志 |
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
2. 时序仅保留 24 小时，长期趋势需外接 TSDB
3. 单实例单进程，无水平扩展设计（40 台规模绰绰有余）
4. 无多用户与权限体系（管理令牌为单一凭据，适合个人/小团队）
5. 告警规则不支持"按节点/分组"定向（当前规则对全部节点生效）

---

## 九、后续路线

| 阶段 | 内容 |
|---|---|
| ~~P1~~ | ~~节点管理 API + 前端管理界面~~ ✅ |
| ~~P2~~ | ~~告警通知（10 通道 + 逐通道测试）~~ ✅ |
| ~~P3~~ | ~~告警规则界面编辑（增删改 + 启停 + 服务端校验）~~ ✅ |
| ~~P4~~ | ~~公开状态页 `/status`（免登录 + 48 段 24h 色块 + 严格脱敏）~~ ✅ |
| ~~P5~~ | ~~数据导出（CSV + Prometheus exposition）~~ ✅ |

**P5 明细**：

*CSV 导出*（需管理令牌，设置页有按钮，也可直接调 API）

| 端点 | 内容 |
|---|---|
| `GET /api/export/assets.csv` | 资产台账 + 当前状态 + 月摊成本合计 |
| `GET /api/export/alerts.csv` | 告警历史（含开始/恢复时间、级别、说明） |
| `GET /api/export/metrics.csv?node=<id>&range=1h\|6h\|24h` | 单节点全量采样（18 列） |

CSV 带 UTF-8 BOM，Excel 打开中文不乱码；文件名含时间戳。

*Prometheus 端点* `GET /metrics`

鉴权：`X-Admin-Token` 头，或 Basic Auth（用户名任意、**密码为管理令牌**）。
抓取配置：

```yaml
scrape_configs:
  - job_name: rollcall
    static_configs: [{ targets: ["host:19080"] }]
    basic_auth: { username: rollcall, password: "<admin.token 内容>" }  # username 任意，密码填令牌
```

暴露的指标族（均带 `id`/`name` 标签）：
`rollcall_build_info`、`rollcall_nodes_total/online`、`rollcall_alerts_active`、
`rollcall_services_total/down`、`rollcall_node_{online,cpu_percent,memory_percent,
disk_percent,load1/5/15,uptime_seconds,tcp_connections,processes,
network_receive_bytes_total,network_transmit_bytes_total,
last_report_age_seconds}`、`rollcall_service_{up,latency_milliseconds,
certificate_days_remaining,uptime_percent_24h}`

---

## 十、实际部署验证

已在 Ubuntu 22.04 / AMD EPYC 16 核 / 31GB 环境完成控制端 + 被控端同机部署验证：

| 项目 | 结果 |
|---|---|
| 服务状态 | rollcall-server / rollcall-agent 均 active + enabled |
| 上报频率 | 严格 30 秒一报，曲线无丢点 |
| 指标正确性 | 与 `df`/`free` 交叉核对无偏差 |
| 数据持久化 | state.json 写入正常，重启后历史保留 |
| 告警引擎 | 4 条默认规则加载，无误报 |
| 安全约束 | Agent 监听端口数 = 0 |
| 内存占用 | server 5.7 MB / agent 7.0 MB |

完整步骤与测试结果见 `docs/部署与验证报告-20261017.txt`，
真实部署界面截图见 `docs/screenshots/deploy-live.png`、`deploy-detail.png`。

---

*License: MIT*

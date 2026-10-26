# Nezha 实机对照（2026-10-26）

对 `43.156.17.202`（腾讯云-新加坡，Nezha 主控）做了一次**只读**检查，目的是看成熟项目
在这些问题上怎么选，而不是抄界面。以下都是实机看到的东西，不是推测。

## 一、部署形态

| 项 | 实际值 |
|---|---|
| 面板 | `ghcr.io/nezhahq/nezha`，版本 **2.3.18**，Docker 单容器，`0.0.0.0:8008->8008` |
| 数据库 | **SQLite**，`/opt/nezha/dashboard/data/sqlite.db` = **2.2 GB**（另有 1.7 GB 手工备份） |
| 反代 | **没有**。1Panel 的 openresty 里没有任何 vhost 指向 8008；面板直接对公网明文 |
| 域名 | `config.yaml` 里 `install_host: data.nat.tf:8008` —— 域名只是解析到 IP:8008，**没有 TLS**（`https: {}` 为空） |
| 备份 | Nezha 自身无定时备份；`config.yaml.bak.*` 是手工 copy。crontab 里的任务是别的站点 |
| 同时暴露 | `3306`(MySQL) / `11211`(memcached) / `8089`(phpMyAdmin) / `1688`(kms) 都在 0.0.0.0 |

> 结论先说：**这台主控不是"域名反代"的范例，而是"直接裸奔 + 域名指过来"**。
> 你在问反代怎么配，Nezha 自己没解决这件事；我们做的 `panelLink` + TLS 反而更完整。

## 二、它的 `config.yaml` 里有什么（去掉敏感项）

```yaml
install_host: data.nat.tf:8008   # 对外地址（= 我们的 panelLink）
agent_secret: <secret>           # 全局共享的 Agent 注册密钥
jwt_secret: <secret>             # 会话签名
jwt_timeout: 1                   # 会话有效期（天）
listen_port: 8008
location: Asia/Shanghai          # 显式时区 ★
language: zh_CN
avg_ping_count: 2                # 延迟取最近 N 次的平均 ★
cover: 1                         # 游客封面页
custom_code: |                   # 任意 HTML/CSS 注入（这里用来隐藏世界地图和顶栏）
https: {}
tsdb: {}
ip_change_notification_group_id: 0
admin_template: admin-dist
user_template: user-dist
```

### 值得抄的三条

1. **`install_host`（对外地址）**：和我们的 `panelLink` 是同一个东西。
   Nezha 早在 v1 就有，说明「反代后地址不对」是这类产品的共性需求 —— 我们 v1.33.0 补上是对的。
2. **`location: Asia/Shanghai`（显式时区）**：日报的"每天 9 点"、免打扰的"22:00–07:00"
   都依赖它。我们现在用**服务器本地时区**，容器里通常是 UTC —— 于是"9 点日报"实际
   发在 17:00。「时区」是我们目前真实存在的缺口。
3. **`avg_ping_count: 2`（延迟平滑）**：单次探测的毛刺不进曲线。
   我们已有方案（中位数 + 峰值带）在待办里，方向一致。

### 不该抄的两条

1. **`custom_code` 任意 HTML 注入**：它靠管理员自觉，本质上是"给后台开一个 XSS 口子"。
   我们的主题包是数据驱动 + 沙箱化的，能力弱一点但不会被一段粘贴毁掉。
2. **`agent_secret` 全局共享密钥**：所有节点共用一个注册密钥，泄露一台=可以伪造任意节点。
   我们是**每节点独立令牌 + HMAC 上报签名**，泄露只影响一台，且可单独轮换 —— 这点不改。

## 三、它的安装链路 vs 我们的

`/opt/nezha/agent/agent.sh`（面板自带的那份）：

```sh
NZ_AGENT_URL="https://github.com/nezhahq/agent/releases/latest/download/nezha-agent_${os}_${os_arch}.zip"
# 国内走 gitee.com；curl --max-time 60
```

- **从 GitHub/Gitee Release 直接下 zip，用 `latest` 不锁版本**
- **没有任何哈希或签名校验**（`grep sha256|checksum` 为空）
- 配置靠环境变量注入 systemd：`NZ_SERVER / NZ_CLIENT_SECRET / NZ_TLS / NZ_DISABLE_*`

对比我们的 `install.sh`：

| 维度 | Nezha | Rollcall |
|---|---|---|
| 下载源 | GitHub / Gitee Release | **控制端自己**（`/agent/download`） |
| 完整性 | 无校验 | 同源 SHA-256 + 长度校验 |
| 版本 | `latest`（随上游漂移） | 控制端分发目录里的确定版本 |
| 服务端发行物 | 不签名 | Ed25519 发行签名（`rollsign`） |
| 上报鉴权 | 共享 `agent_secret` | **每节点令牌 + 时间戳/nonce/HMAC** |
| 服务端身份 | 无（明文 HTTP） | 内置 TLS + **证书指纹固定** |

实测过的地方：上海节点拉 GitHub 反复被截断，我们的"从控制端下载"直接绕过了这个坑；
Nezha 那种写法在那台机器上会装不上。

## 四、它的运维债（我们不要重演）

1. **SQLite 2.2 GB 且无清理**：`service_histories` 默认不裁剪，面板越用越慢、备份越来越重。
   我们在设计时就做了分层保留（24h 原始 / 7d 1m / 30d 5m / 365d 1h）。
2. **面板明文直接对公网**：登录密码与节点密钥都在链路上裸奔；配合"全局 `agent_secret`"，
   一旦被中间人拿到就能伪造节点。这是我们已经关闭的 P0。
3. **`memcached 11211` 对公网**：不仅未授权可读写，还是现成的 UDP/TCP 反射放大源。
   建议直接删掉端口映射，或只绑 `127.0.0.1`。

## 五、由此产生的待办（按价值排序）

1. **显式时区设置**（`location` 的等价物）：设置页给一个 IANA 时区，日报/周报/免打扰/
   到期提醒都按它算。当前依赖服务器本地时区，容器里默认 UTC 会让"9 点日报"错 8 小时。
2. **延迟平滑**（`avg_ping_count` 的等价物）：已在中位数方案里，一并做。
3. 游客封面页（`cover`）：公开状态页已有，是否要"未授权也看得到一页概览"待定，
   优先级低 —— 我们的公开页默认就开着，卖点已经覆盖。

## 六、把数据库摊开看：它到底有哪些功能

看 `sqlite.db` 的表结构比看界面可靠。以下是实机读到的表与行数（主控在跑 32 台）：

| 表 | 行数 | 说明 |
|---|---|---|
| `servers` | 32 | 节点台账 |
| `services` | 6 | 服务探测 |
| `service_histories` | **10 789 694** | 探测历史（这是 2.2 GB 的来源） |
| `transfers` | 248 | 按月流量记录（32 台 × 约 8 个月） |
| `alert_rules` | 0 | 未启用 |
| `notifications` / `notification_groups` / `notification_group_notifications` | 0 | 通知渠道与**通知组** |
| `api_tokens` | 0 | API 令牌 |
| `crons` | 0 | 定时任务（向 Agent 下发） |
| `ddns` | 0 | DDNS 配置 |
| `nats` | 0 | NAT/隧道信息展示 |
| `nz_waf` | 0 | IP 封禁表（WAF） |
| `jwt_sessions` | 0 | 会话 |
| `oauth2_binds` | 0 | 第三方登录绑定 |
| **`mcp_audit_logs`** | 0 | **MCP 工具调用审计** |
| `users` | 1 | 用户（带 `role` 与 `token_version`） |

### 几个字段值得单独说

`servers`：`note`（内部备注）、`public_note`（公开备注）、`display_index`（手工排序）、
`hide_for_guest`（对游客隐藏）、`enable_d_dns` / `ddns_profiles_raw`。

`services`：`skip_servers_raw`（**跳过哪些节点**，说明它的探测是从 Agent 侧发起的）、
`duration`、`notification_group_id`（**按服务选通知组**）、`cover`（公开页是否展示）、
`enable_trigger_task` + `fail_trigger_tasks_raw`（**失败时触发定时任务**）、
`min_latency` / `max_latency` / `latency_notify`（延迟上下限告警）。

`api_tokens`：`scopes_csv`、**`servers_csv`（按服务器限定范围）**、
`expires_at`、`last_used_at`、**`last_used_ip`**。

`mcp_audit_logs`：`tool` / `server_id` / `args_hash` / `args_peek` /
`outcome` / `error_code` / `duration_ms` / `ip`。

## 七、这一轮照它补的（v1.34.0）

| 项 | 它怎么做 | 我们怎么做 | 为什么 |
|---|---|---|---|
| **规则级通知路由** | `services.notification_group_id` + 通知组表 | `AlertRule.Channels`（渠道 ID 列表），空=全部 | 我们有多个渠道时，"磁盘告警发给谁、登录告警发给谁"只能全局一刀切 |
| **MCP/令牌访问审计** | `mcp_audit_logs` 全字段落库 | 令牌的**读**访问记入现有审计（`api.read`），5 分钟内同令牌同路径合并 | MCP 是"用令牌调只读 API"，审计做在令牌鉴权处能同时覆盖 MCP 和任何脚本 |
| **令牌最近来源** | `api_tokens.last_used_ip` | `APIToken.LastUsedIP` | 令牌泄露时"从哪儿被用"比"什么时候"更好定位 |

实现时踩到一处**自己的漏洞**：引擎有三条投递路径（`send` / `emit` / `emitMerged`），
只在 `send` 接线时，**离线类告警（走批量路径）会静默丢失渠道选择** —— 而离线恰恰是最常见的告警。
端到端测试当场抓到了它（`TestAlertCarriesRuleChannels` 报 `实际 []`）。

### 明确不抄

- **`crons` + `fail_trigger_tasks`**：失败时向 Agent 下发命令 = 服务端变成指令下发通道，
  与本项目"Agent 只读、服务端不下发"的边界冲突（见 README 的安全模型）。
- **`custom_code`**：任意 HTML/JS 注入，等价于给后台开 XSS 口子。
- **`agent_secret` 全局密钥**：泄露一台即可伪造任意节点。
- **`nz_waf` 的 IP 封禁表**：我们的登录限速是内存态、按 IP+路由分桶，够用；
  另做持久封禁表意味着要处理"误封自己"的救援路径，收益不成比例。
- **`ddns`**：节点 IP 变更我们已经推送通知（10 分钟冷却、跨 /24 抬到 critical），
  自动改 DNS 是另一件事，且要引入第三方凭据。

### 还在评估

- **`display_index`（手工排序）**：对 40 台以上的台账有意义，等节点数再涨。
- **`hiddenForGuest` 的细粒度**：我们只有"公开页隐藏节点"整节点开关，它是按节点标记。
- **`oauth2_binds`**：单人使用价值低，且引入外部身份源。
- **`users.role` 多用户**：同上，暂不做。

## 八、一个可以直接借用的"用法"（不是代码）

它那 6 个服务全是 `type=3`（TCP），目标是 `hb-cu-v4.ip.zstaticcdn.com:80`、
`bj-cm-v4...`、`bj-ct-v4...` —— **用各省运营商的 CDN 节点当探测目标**，
量的是"控制端到各省各运营商的线路质量"，而不是"某个业务是否在线"。

我们的 TCP 探测可以直接这么用（`target` 填 `host:port`）：

```
北京联通 bj-cu-v4.ip.zstaticcdn.com:80
北京移动 bj-cm-v4.ip.zstaticcdn.com:80
北京电信 bj-ct-v4.ip.zstaticcdn.com:80
湖北联通 hb-cu-v4.ip.zstaticcdn.com:80
```

配上 `latency` / `jitter` / `loss` 规则，就能回答"这台机器回国内哪条线路变差了"。
注意：这类目标**只测通不通与延迟**，不代表任何业务健康度，别和服务可用性混在一个面板里看。

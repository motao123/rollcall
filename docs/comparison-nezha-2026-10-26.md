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

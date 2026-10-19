# 运维手册

面向已按 README 完成部署的 Rollcall 实例（v1.8+）。覆盖日常备份、升级回滚、签名自更新、账号/令牌管理、插件与故障排查。

---

## 1. 部署布局

| 项目 | 默认位置 | 说明 |
|---|---|---|
| 服务端二进制 | `/opt/rollcall/rollcall-server` | systemd 单元 `rollcall-server.service` |
| 服务端数据 | `/opt/rollcall/data` | `state.json`（主快照）、`history.json`（分层历史）、`admin.token`（自动化令牌）、`admin-user.json`（面板账号 PBKDF2 哈希）、`session.key`（会话签名密钥），权限 0600 |
| 服务端单元 | `/etc/systemd/system/rollcall-server.service` | 以 `rollcall` 用户运行 |
| Agent 二进制 | `/usr/local/bin/rollcall-agent` | systemd 单元 `rollcall-agent.service` |
| Agent 配置 | `/etc/rollcall/agent.json` | `root:rollcall-agent 0640` |
| Agent 单元 | `/etc/systemd/system/rollcall-agent.service` | 以 `rollcall-agent` 用户运行 |

查看状态与日志：

```bash
systemctl status rollcall-server rollcall-agent --no-pager
journalctl -u rollcall-server -n 100 --no-pager
journalctl -u rollcall-agent -n 100 --no-pager
```

---

## 2. 版本与升级

```bash
# 构建时注入版本号（默认取 git describe）
make linux VERSION=v1.9.2
# 或直接使用：
./rollcall-server -version
./rollcall-agent -version
```

升级步骤（务必先备份）：

```bash
sudo cp /opt/rollcall/data/state.json \
        /var/backups/rollcall/state-$(date +%F-%H%M).json.gz
sudo cp /opt/rollcall/data/history.json \
        /var/backups/rollcall/history-$(date +%F-%H%M).json.gz
sudo systemctl stop rollcall-server

# 替换二进制与单元文件后
sudo install -m 0755 rollcall-server /opt/rollcall/rollcall-server
sudo install -m 0644 deploy/rollcall-server.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl start rollcall-server
curl -fsS http://127.0.0.1:19080/healthz
```

Agent 升级：

```bash
sudo install -m 0755 rollcall-agent /usr/local/bin/rollcall-agent
sudo install -m 0644 deploy/rollcall-agent.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl restart rollcall-agent
```

### 快照版本与回滚

`state.json` 带 `schemaVersion`（当前为 3；v3 仅提升版本号，历史聚合存放在独立的 `history.json`），
`history.json` 自带独立版本号（当前为 2；schema 2 为聚合桶增加内存/磁盘绝对用量总和，
v1.7 可直接读取 schema 1 并自动补齐）。旧程序遇到更高版本时会：

1. 把文件改名为 `state.json.newer-<时间>` / `history.json.newer-<时间>`；
2. 拒绝启动并在日志中说明需要升级服务端。

因此**回滚旧二进制前，必须先恢复升级前备份的两份文件**，否则旧程序会拒绝载入新格式数据：

```bash
sudo systemctl stop rollcall-server
sudo cp /var/backups/rollcall/state-<升级前时间>.json.gz /opt/rollcall/data/state.json
sudo cp /var/backups/rollcall/history-<升级前时间>.json.gz /opt/rollcall/data/history.json
sudo chown rollcall:rollcall /opt/rollcall/data/state.json /opt/rollcall/data/history.json
sudo chmod 0600 /opt/rollcall/data/state.json /opt/rollcall/data/history.json
# 换回旧二进制后启动
sudo systemctl start rollcall-server
```

### Agent 签名自更新（v1.7+，默认关闭）

Agent 只从**本机配置的清单地址**主动拉取更新；服务端不参与，面板也无法触发。
更新包与清单都由离线私钥签名，Agent 侧用内置公钥验签。

**1. 生成密钥（只在一台受控机器上）**

```bash
make rollsign
./rollsign keygen -out /root/rollcall-update.key
# 输出 base64 公钥，填入 Agent 配置的 update.publicKey
```

私钥文件权限为 0600，**不能提交到仓库或复制到被监控节点**。

**2. 构建并签名清单**

```bash
make linux VERSION=v1.9.2
./rollsign sign -key /root/rollcall-update.key -version v1.9.2 \
  -base-url https://updates.example.com/agent/v1.9.2 \
  -asset linux-amd64=dist/rollcall-agent-linux-amd64 \
  -asset linux-arm64=dist/rollcall-agent-linux-arm64 \
  -out dist/manifest.json
```

把两个 Agent 二进制和 `manifest.json` 上传到 `base-url` 对应目录；
资产 URL 由 `base-url + 文件名` 生成，Agent 只挑选与自身平台匹配的资产。
`-allow-insecure` 仅用于本机 HTTP 测试，生产必须是 HTTPS。

**3. 配置 Agent**

```json
{
  "update": {
    "enabled": true,
    "manifestUrl": "https://updates.example.com/agent/manifest.json",
    "publicKey": "<base64 Ed25519 公钥>",
    "checkIntervalHours": 6
  }
}
```

`checkIntervalHours` 范围 1–168，默认 6。启用后 Agent 在启动 30 秒后首次检查，
之后按间隔轮询；也可手动执行一次：

```bash
sudo runuser -u rollcall-agent -- \
  /opt/rollcall-agent/rollcall-agent -config /etc/rollcall/agent.json -update-now
```

**4. 目录写权限（关键）**

Agent 需要能创建临时文件并 rename 覆盖自身。默认的 `/usr/local/bin` 归 root，
且单元启用了 `ProtectSystem=strict`，所以**必须同时满足文件权限与 systemd 放行**：

```bash
sudo install -d -o rollcall-agent -g rollcall-agent -m 0750 /opt/rollcall-agent
sudo install -m 0755 -o rollcall-agent -g rollcall-agent \
  dist/rollcall-agent-linux-amd64 /opt/rollcall-agent/rollcall-agent
```

```ini
# /etc/systemd/system/rollcall-agent.service.d/self-update.conf
[Service]
ExecStart=
ExecStart=/opt/rollcall-agent/rollcall-agent -config /etc/rollcall/agent.json
ReadWritePaths=/opt/rollcall-agent
```

```bash
sudo systemctl daemon-reload
sudo systemctl restart rollcall-agent
```

只有该目录可写；`/proc`、配置与其余系统路径仍保持只读。

**5. 安装、验证与回滚**

- 下载完成后校验大小、SHA-256、ELF 魔数与架构，再原子替换；
  旧二进制保存为可执行文件同目录的 `rollcall-agent.prev`
- 替换后写入 `rollcall-agent.update-pending`，`syscall.Exec` 重启；
  新版本首次成功上报后删除标记
- 新版本连续 3 次启动仍未成功上报：自动恢复 `.prev`，并把该版本记入
  7 天冷却（期间不会再次自动安装）
- 手动回滚（systemd 一次性操作，保留失败版本冷却）：

```bash
sudo tee /etc/systemd/system/rollcall-agent.service.d/rollback.conf >/dev/null <<'EOF'
[Service]
ExecStart=
ExecStart=/opt/rollcall-agent/rollcall-agent -config /etc/rollcall/agent.json -rollback
EOF
sudo systemctl daemon-reload
sudo systemctl restart rollcall-agent
# 进程恢复旧版并 re-exec（参数中已去掉 -rollback）后继续运行；随后移除 override
sudo rm /etc/systemd/system/rollcall-agent.service.d/rollback.conf
sudo systemctl daemon-reload
```

若二进制已无法启动（例如动态加载错误），直接停服务并用备份覆盖：

```bash
sudo systemctl stop rollcall-agent
sudo install -m 0755 -o rollcall-agent -g rollcall-agent \
  /opt/rollcall-agent/rollcall-agent.prev /opt/rollcall-agent/rollcall-agent
sudo systemctl start rollcall-agent
```

常见失败原因：`更新目录不可写`（权限或 `ReadWritePaths` 未配置）、
`签名校验失败`（公钥/清单不匹配）、`架构不匹配`（上传了错误平台的资产）、
`拉取更新清单失败`（HTTPS 证书、DNS 或地址不可达）。全部只记录日志，不影响当前上报。

---

## 3. 备份与恢复

`state.json` 通过「写 `.tmp` → rename」原子替换，因此**运行中复制也是完整快照**（可能是最近一次自动保存，默认 60 秒前）。
`history.json` 同样原子替换，落盘间隔为 5 分钟：

```bash
sudo install -d -m 0700 -o root -g root /var/backups/rollcall
sudo cp /opt/rollcall/data/state.json \
        /var/backups/rollcall/state-$(date +%F-%H%M).json.gz
sudo cp /opt/rollcall/data/history.json \
        /var/backups/rollcall/history-$(date +%F-%H%M).json.gz
```

需要连管理令牌、账号哈希与会话密钥一起备份时打包整个数据目录，并确保备份文件不进 Git：

```bash
sudo tar -C /opt/rollcall -czf /var/backups/rollcall/data-$(date +%F).tgz data
sudo chmod 0600 /var/backups/rollcall/data-$(date +%F).tgz
```

恢复：

```bash
sudo systemctl stop rollcall-server
sudo cp /var/backups/rollcall/state-<时间>.json.gz /opt/rollcall/data/state.json
sudo cp /var/backups/rollcall/history-<时间>.json.gz /opt/rollcall/data/history.json
sudo chown rollcall:rollcall /opt/rollcall/data/state.json /opt/rollcall/data/history.json
sudo chmod 0600 /opt/rollcall/data/state.json /opt/rollcall/data/history.json
sudo systemctl start rollcall-server
```

> 快照损坏时服务端不会以空库启动，而是把文件隔离为 `state.json.corrupt-<时间>`。
> 确认备份可用后再删除隔离文件，否则会丢失全部节点与令牌。

### 历史数据分层

| 分辨率 | 默认保留期 | 用途 |
|---|---|---|
| 原始 30s | 24 小时（固定） | 1h / 6h / 24h 查询 |
| 1 分钟 | 7 天 | 7d 查询 |
| 5 分钟 | 30 天 | 30d 查询 |
| 1 小时 | 365 天 | 365d 查询 |

三级聚合保留期可用启动参数调整：

```bash
-addr 127.0.0.1:19080 -data /opt/rollcall/data \
  -history-1m-retention 168h \
  -history-5m-retention 720h \
  -history-1h-retention 8760h
```

- 必须满足 `1m ≤ 5m ≤ 1h`，且分别是 60s / 300s / 3600s 的整数倍
- 单层上限 10 年且不超过 1,000,000 个桶；配置非法时服务端拒绝启动
- 调整保留期后历史文件立即按新上限裁剪；调大不会凭空补出旧数据
- 聚合桶在内存中惰性分配，40 节点满保留期约占 70MB；大幅调大前先确认内存
- schema 2 起桶内保存内存/磁盘绝对用量总和，扩容后长范围平均值仍然准确；
  旧桶自动回退到百分比换算，无需迁移
- 历史每 5 分钟写入 `history.json`；进程崩溃最多丢失该窗口的聚合点，
  重启时会用 `state.json` 中的 24h 原始点回放补齐
- `history.json` 缺失或损坏时，会自动从 24h 原始点重建；损坏文件隔离为
  `history.json.corrupt-<时间>`，无需人工干预
- 节点被删除时，其聚合历史一并删除
- 回滚到 v1.6 或更早版本前，必须同时恢复备份的 `state.json` 与 `history.json`（见上一节）；
  v1.6 不识别 history schema 2，会将其隔离并重建

---

## 4. 账号、会话与令牌管理

### 管理员账号

面板登录只接受「用户名或邮箱 + 密码」。账号保存在 `data/admin-user.json`（0600，schema 1），
文件内只有 PBKDF2-HMAC-SHA256 哈希（600,000 次迭代、16 字节随机盐），没有明文密码。
用户名与邮箱均不区分大小写。

初始化方式（三选一，仅在首次启动、账号文件不存在时生效）：

```bash
# 1) 环境变量（推荐，写进 systemd 的 EnvironmentFile）
ROLLCALL_ADMIN_PASSWORD='至少 12 位的强密码'

# 2) 启动参数
-admin-user admin -admin-email ops@example.com -admin-password '至少 12 位的强密码'

# 3) 都不提供：服务端生成 24 位随机密码，仅在首次启动的日志中打印一次
journalctl -u rollcall-server | grep '面板管理员已创建'
```

重置密码（服务停止时执行；命令重置后立即退出，不会启动服务）：

```bash
read -rsp '新密码: ' PW && echo          # 从密码管理器粘贴，不进入 shell history
sudo systemctl stop rollcall-server
sudo runuser -u rollcall -- env ROLLCALL_ADMIN_PASSWORD="$PW" \
  /opt/rollcall/rollcall-server -data /opt/rollcall/data -reset-admin-password
unset PW
sudo systemctl start rollcall-server
```

重置会更新会话版本，旧 Cookie 全部失效，需要用新密码重新登录。
密码至少 12 位；也可用 `-admin-password` 传参，但会留在 shell history，不推荐。
用环境变量完成首次初始化后，建议登录并改成自己的密码，然后清除
`/etc/rollcall/server.env` 中的密码行——环境变量只在创建/重置时读取，
正常启动不会覆盖已保存的哈希。

### 会话

- 登录成功后签发 `rollcall_session` Cookie：HMAC-SHA256 签名，`HttpOnly`、`SameSite=Strict`，
  HTTPS（含 `X-Forwarded-Proto: https`）下自动带 `Secure`；有效期 7 天。
- 签名密钥在 `data/session.key`（32 字节，0600），首次启动自动生成，因此**重启服务不会掉登录态**。
  删除或替换该文件会让所有会话立即失效，可作为「强制全员重新登录」手段。
- 连续 10 次登录失败后，来源 IP 封禁 10 分钟；成功登录清零计数。
- 会话鉴权下的写操作必须带 `X-Requested-With: XMLHttpRequest`，用于阻断跨站表单提交；
  `X-Admin-Token` 请求不受此限制（自定义头本身已阻止表单跨站）。

### 管理令牌

位于 `data/admin.token`（0600，默认 48 位十六进制）。轮换步骤：

```bash
sudo systemctl stop rollcall-server
sudo sh -c 'openssl rand -hex 24 > /opt/rollcall/data/admin.token'
sudo chown rollcall:rollcall /opt/rollcall/data/admin.token
sudo chmod 0600 /opt/rollcall/data/admin.token
sudo systemctl start rollcall-server
```

轮换后需同步更新 Prometheus `basic_auth` 密码及所有脚本；面板登录不使用该令牌，无需改浏览器配置。**不要把令牌提交到仓库或贴进聊天记录。**

### 节点令牌

在「设置 → 节点管理 → 接入」弹窗中点击「轮换令牌」，或调用 API：

```bash
curl -X POST http://127.0.0.1:19080/api/admin/nodes/<id>/rotate-token \
  -H "X-Admin-Token: $(sudo cat /opt/rollcall/data/admin.token)"
```

旧令牌立即失效。随后在目标机器更新 `/etc/rollcall/agent.json` 并重启 Agent：

```bash
sudo systemctl restart rollcall-agent
sudo runuser -u rollcall-agent -- \
  /usr/local/bin/rollcall-agent -config /etc/rollcall/agent.json -once
```

---

## 5. 自动保存与快照

- `-save-interval` 控制自动保存间隔，默认 `60s`，允许范围 `10s–1h`。
- 收到 SIGTERM/SIGINT 时总会立即保存一次，systemd 重启不会丢最近一次变更。
- 快照为 gzip 压缩 JSON，兼容旧版明文 JSON；首次保存后转为 gzip。
- 损坏或更高 schema 版本会被隔离，服务端拒绝以空库启动。

---

## 6. 私网探测与通知（显式开启）

默认拒绝回环/私网/链路本地/CGNAT 目标。确需监控本机服务或向内网 webhook 推送时，**不要直接改仓库模板**，用 systemd drop-in 保留升级时的默认安全值：

```bash
sudo install -d /etc/systemd/system/rollcall-server.service.d
sudo tee /etc/systemd/system/rollcall-server.service.d/private-targets.conf >/dev/null <<'EOF'
[Service]
ExecStart=
ExecStart=/opt/rollcall/rollcall-server -addr 127.0.0.1:19080 -data /opt/rollcall/data -allow-private-probes -allow-private-notify
EOF
sudo systemctl daemon-reload
sudo systemctl restart rollcall-server
```

两个开关应按需取舍：只探测本机就只加 `-allow-private-probes`；不要为省事同时长期开启。

---

## 7. 告警规则定向与通知投递

- 规则支持 `nodeIds` 与 `groups`（界面在规则弹窗中配置），空表示对所有节点生效；两者同时存在时按「或」匹配。
- 规则收紧或改向后，原先命中但已不在范围内的活跃告警会自动关闭。
- 默认规则只在全新实例上播种一次；删光规则后重启不会复活（持久化标记 `rulesInitialized`）。
- 告警只负责把通知写入持久化 Outbox，由 **8** 个 worker 异步投递；任务随快照落盘，重启后继续投递。
- Outbox 队列上限 **2000** 条。队列满且无法清理时新任务会被拒绝，服务端日志记录
  `通知队列已满（2000 条），拒绝新任务: ...`，并累计到 `rollcall_notify_rejected_total`。
  若该计数持续增长，说明通道响应慢或告警过于密集，应先修通道。
- 队列深度、送达/失败/重试/静默拦截/队列满拒绝与最近成功时间见 `/metrics` 与告警页。
- 设置页「通知队列」列出最近 100 条任务及最近错误；修复通道后可对失败/被拦截任务手动重试，
  重试会重置尝试次数并立即唤醒调度器。
- 规则停用、删除或收紧定向范围时，被关闭的活跃告警会补发恢复通知（正文注明原因），不再静默消失。

### 通知插件（v1.7+，默认关闭）

只有服务端显式指定 `-notify-plugins <目录>` 时才会加载插件；不指定则没有执行外部程序的路径。
目录顶层每个 `*.json` 对应一个通道，任一清单非法都会导致服务端拒绝启动。

示例清单 `/opt/rollcall/plugins/local-alert.json`：

```json
{
  "type": "local_alert",
  "label": "本地告警脚本",
  "desc": "把告警转交给内网自动化平台",
  "command": "send.sh",
  "args": ["--format", "json"],
  "timeout": 10,
  "fields": [
    {
      "key": "endpoint",
      "label": "接收地址",
      "type": "url",
      "required": true,
      "placeholder": "https://automation.internal/hook"
    },
    {
      "key": "token",
      "label": "鉴权 Token",
      "type": "password",
      "secret": true
    }
  ],
  "env": ["ALERT_CHANNEL=rollcall"]
}
```

`send.sh` 必须位于插件目录内且可执行：

```bash
#!/bin/sh
set -eu
payload=$(cat)   # stdin 是 JSON：channel.options / subject / body / message
# 用 jq/python 解析后投递；退出码 0 表示成功，非 0 视为失败
printf '%s\n' "$payload" >/dev/null
```

投递契约：

- stdin JSON 包含 `channel.id`、`channel.name`、`channel.options`、`subject`、`body`、`message`
- 进程环境**完全重建**：固定 `PATH`、`LANG`、`HOME`，加上清单中的 `env`；
  不继承服务端环境变量，因此管理令牌等不会泄露给插件
- `command` 必须是相对路径的普通可执行文件；绝对路径、`..`、符号链接越界、全局可写文件都会被拒绝
- 插件类型不能与内置通道或其它插件重复
- 超时 1–30 秒（默认 10），stdout/stderr 合计最多 8KB；
  失败时错误摘要写入通知任务 `lastError`，不会回显完整输出
- 插件以服务端用户（默认 `rollcall`）运行，属于**受信任本地扩展，不是沙箱**

启用方式（systemd drop-in）：

```bash
sudo install -d -o root -g rollcall -m 0755 /opt/rollcall/plugins
sudo install -m 0755 send.sh /opt/rollcall/plugins/send.sh
sudo install -m 0644 local-alert.json /opt/rollcall/plugins/local-alert.json
```

```ini
# /etc/systemd/system/rollcall-server.service.d/notify-plugins.conf
[Service]
ExecStart=
ExecStart=/opt/rollcall/rollcall-server -addr 127.0.0.1:19080 -data /opt/rollcall/data \
  -allow-private-probes -allow-private-notify \
  -notify-plugins /opt/rollcall/plugins
```

修改清单或脚本后需重启服务端。插件会出现在通知设置的通道类型列表中，可像内置通道一样
配置、脱敏与逐通道测试。目录与文件不要设为全局可写。

> 若已有其它 drop-in 也重写了 `ExecStart`（例如私网放行），后加载的定义会覆盖先前定义。
> 多个 drop-in 并存时，应把完整命令行合并到同一个文件中维护，避免升级后丢失参数。

---

## 8. 外观主题

设置页「外观主题」支持：明暗模式（`auto`/`light`/`dark`）、浅色与深色强调色（`#rrggbb`）、
圆角（0–24px）以及紧凑密度。

- 服务端只接受上述固定字段，未知字段或非法值直接返回 400；**不接受任何 CSS 文本**
- 前端仅把主题值映射到白名单 CSS 变量，公开状态页同步应用，不新增数据暴露
- 主题保存在 settings 中：`GET /api/theme` 公开读取（仅样式）；`GET/PUT /api/admin/theme`
  需要管理令牌或已登录会话
- 设置页的「重置」会恢复默认主题；强调色以可读性为准，避免与文字/背景对比不足

---

## 9. 故障排查

| 现象 | 排查方向 |
|---|---|
| 浏览器反复跳回登录页 / 401 | 会话 Cookie 被清除，或 `data/session.key` 被删除/替换；管理 API 直连需 `X-Admin-Token`，节点上报需 `X-Node-Token`，三者不可混用 |
| 登录提示「用户名或密码错误」 | 用户名或邮箱不区分大小写，但仍需完全匹配；忘记密码按 §4 重置 |
| 登录后写操作返回 403 | 会话鉴权的写请求缺少 `X-Requested-With: XMLHttpRequest`（浏览器扩展/脚本常见）；管理令牌请求不受此限制 |
| 重置密码后仍能旧会话访问 | `-reset-admin-password` 需在服务停止时执行，且随后重启服务才生效 |
| 管理接口无令牌返回 401 | 未登录会话且未带管理令牌；先登录面板或按 §4 配置令牌 |
| 节点显示离线 | 服务端超过 300 秒未收到上报；120–300 秒显示「数据延迟」，从未上报显示「待接入」。检查 Agent 日志、时间同步、`interval ≤ 60` |
| Agent 拒绝启动（HTTP） | 非本机服务端默认必须 HTTPS；临时接受风险可加 `-allow-insecure` |
| Agent 读取配置失败 | 确认 `/etc/rollcall` 属组 `rollcall-agent`(0750)、`agent.json` 为 `root:rollcall-agent 0640` |
| 服务探测全部失败 | 目标是否为私网？需显式 `-allow-private-probes`；自签名证书需 `-insecure-probe-tls` |
| 通知发不出去 | 设置页逐通道测试；自建内网服务需 `-allow-private-notify`；查看日志中的丢弃提示 |
| 启动报快照错误 | 查看日志中的 `.corrupt-*` / `.newer-*` 备份路径；修复或恢复备份后再启动 |
| 自更新未生效 | 确认 `update.enabled`、清单 HTTPS/签名公钥；检查日志中的签名、架构、目录不可写错误；systemd 需 `ReadWritePaths` |
| 插件通道未出现 | 是否带 `-notify-plugins`；清单/命令是否在目录内、可执行且非全局可写；服务端启动日志会指出具体清单错误 |
| 启动报历史保留期无效 | 保留期必须满足 1m ≤ 5m ≤ 1h、为对应窗口整数倍、单层不超过 10 年与 100 万桶 |
| 端口被占用 | `ss -ltnp \| grep 19080`；确认没有重复启动旧实例 |

健康检查与最小验证：

```bash
curl -fsS http://127.0.0.1:19080/healthz          # ok
curl -fsS http://127.0.0.1:19080/api/status | head # 公开状态数据
curl -fsS http://127.0.0.1:19080/metrics \
  -H "X-Admin-Token: $(sudo cat /opt/rollcall/data/admin.token)" | head
# 重点观察：rollcall_save_failures_total、rollcall_save_duration_seconds、
# rollcall_history_points、rollcall_notify_rejected_total、rollcall_nodes_{stale,offline,no_data}
```

---

## 10. 上线后检查清单

- [ ] `rollcall-server -version` / `rollcall-agent -version` 与发布版本一致
- [ ] 两个 systemd 单元均为 `active (running)` 且已 `enable`
- [ ] `/healthz` 返回 `ok`，`/status` 可匿名访问且不含主机名/IP/价格
- [ ] 目标节点在线，30–60 秒内有新采样
- [ ] 可用用户名与邮箱分别登录面板；错误密码被拒绝；刷新与重启服务后登录态保持；退出后回到登录页
- [ ] 管理令牌 API 与 `/metrics` 抓取仍可用，`data/admin.token` 权限为 0600
- [ ] 已配置备份任务，且实际恢复演练过一次
- [ ] 管理员密码已存入密码管理器，非默认/弱口令；`admin-user.json` 中只有哈希
- [ ] 管理令牌、节点令牌、`admin-user.json`、`session.key` 均未进入仓库、日志或截图
- [ ] 如启用自更新：`update.manifestUrl` 为 HTTPS、公钥正确、二进制目录可写且 systemd 已放行
- [ ] 如启用通知插件：目录与清单非全局可写，逐通道测试通过
- [ ] 如修改主题：管理页与 `/status` 均按预期显示，重置后恢复默认

# 运维手册

面向已按 README 完成部署的 Rollcall 实例（v1.10+）。覆盖日常备份、升级回滚、签名自更新、账号/令牌管理、插件与故障排查。

---

## 1. 部署布局

| 项目 | 默认位置 | 说明 |
|---|---|---|
| 服务端二进制 | `/opt/rollcall/rollcall-server` | systemd 单元 `rollcall-server.service` |
| 服务端数据 | `/opt/rollcall/data` | `state.json`（主快照）、`history.json`（分层历史）、`admin.token`（自动化令牌）、`admin-user.json`（面板账号 PBKDF2 哈希）、`session.key`（会话签名密钥），权限 0600 |
| 初始密码 | `/opt/rollcall/data/initial-admin-password.txt` | 仅在未提供初始密码时生成（0600）；首次成功登录后自动删除 |
| 服务端单元 | `/etc/systemd/system/rollcall-server.service` | 以 `rollcall` 用户运行 |
| Agent 二进制 | `/usr/local/bin/rollcall-agent` | systemd 单元 `rollcall-agent.service` |
| Agent 配置 | `/etc/rollcall/agent.json` | `root:rollcall-agent 0640` |
| Agent 单元 | `/etc/systemd/system/rollcall-agent.service` | 以 `rollcall-agent` 用户运行 |
| Agent 分发目录 | `/opt/rollcall/agent` | 仅启用 `-agent-dir` 时存在；含 amd64/arm64/arm/386 二进制，供一键安装与升级下载 |
| TLS 证书（v1.29.0+） | `/etc/rollcall/tls/` | **当前生产（2026-10-26 起）已改由 Caddy 终止 TLS**（ACME 自动签发续期，见 7.6 末尾），本目录证书与 `tls.conf` drop-in 已移除（恢复内置 TLS 见该节回滚说明） |

### 容量基线

| 规模 | CPU | 内存 | 磁盘 | 说明 |
|---|---|---|---|---|
| ≤10 节点 | 1 vCPU | 512MB | 2GB | 起步配置 |
| ~40 节点 | 1 vCPU | 1.5–2GB | 5GB | 24h 环跑满后实测 RSS 约 361MB，快照期间还需峰值余量 |
| >40 节点 / 更短 interval | 2 vCPU | 4GB+ | 按实测 | 先用满环数据压测，再评估拆分实例 |

内存必须按**满环稳态**估算：空环会低估一个数量级，原始点约 5–9MB/节点。
磁盘占用主要由 `history.json` 与快照频率决定，长期运行建议给 `data/` 单独挂载数据盘，
并监控 `data/` 体积与 `/readyz`（数据目录不可写会直接返回 503）。
仓库内 `bench/bench_steady.py` 可复现完整数据，README「十、容量基线」有 Linux 实测表。

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
make linux VERSION=v1.13.1
(cd dist && sha256sum -c SHA256SUMS)
# 或直接使用：
./rollcall-server -version
./rollcall-agent -version
```

不想在目标机上装 Go，也可以直接取 CI 随 tag 发布的预编译产物（`v1.10.5` 起）：

```bash
VERSION=v1.13.1; ARCH=amd64
BASE="https://github.com/motao123/rollcall/releases/download/${VERSION}"
curl -fsSLO "$BASE/rollcall-server-linux-$ARCH"
curl -fsSLO "$BASE/rollcall-agent-linux-$ARCH"
curl -fsSLO "$BASE/SHA256SUMS"
sha256sum -c --ignore-missing SHA256SUMS
```

构建统一带 `-trimpath -buildvcs=false`：同一份源码 + 相同 Go 版本，在 git 工作区、
tag 克隆或普通目录中构建出的二进制字节一致。发布时同时记录版本号、Go 版本与
`dist/SHA256SUMS`，便于核对线上文件是否来自该次构建。

升级步骤（务必先备份）：

```bash
sudo install -d -m 0700 /var/backups/rollcall
sudo cp /opt/rollcall/data/state.json \
        /var/backups/rollcall/state-$(date +%F-%H%M).json
sudo cp /opt/rollcall/data/history.json \
        /var/backups/rollcall/history-$(date +%F-%H%M).json
sudo systemctl stop rollcall-server
sudo cp -p /opt/rollcall/rollcall-server /opt/rollcall/rollcall-server.prev

# 替换二进制与单元文件后
sudo install -m 0755 rollcall-server /opt/rollcall/rollcall-server
sudo install -m 0644 deploy/rollcall-server.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl start rollcall-server
curl -fsS http://127.0.0.1:19080/readyz
/opt/rollcall/rollcall-server -version
```

服务端回滚：停服务，把 `/opt/rollcall/rollcall-server.prev` 装回原位后启动。
若新版本已把快照写成更高 schema，还必须先恢复上面备份的 `state.json` / `history.json`（见下节）。

### 一键升级/回滚脚本（v1.10+，v1.14 起支持发行验签）

`deploy/rollcall-server-upgrade.sh` 封装了「校验 → 备份 `.prev` → 原子替换 → 重启 → 健康检查 → 失败自动回滚」。
v1.14 起，配置了发行公钥的实例会在替换前强制校验 Ed25519 发行签名。

**一次性配置发行签名**（签名机与控制端分开）：

```bash
# 签名机：生成独立的发行密钥（不要复用 Agent 更新私钥，也不要把私钥放到控制端）
make rollsign
./rollsign keygen -out /root/rollcall-release.key
./rollsign pubkey -key /root/rollcall-release.key -out /root/rollcall-release.pub

# 构建并签名服务端产物；-version 必须与 VERSION 完全一致（含 v 前缀）
make linux VERSION=v1.15.0
make sign-release ROLLSIGN_KEY=/root/rollcall-release.key VERSION=v1.15.0
# 生成 dist/rollcall-server-linux-<arch>.sig，与对应二进制一起分发

# 控制端：安装公钥与本机验签工具
sudo install -d -m 0755 /etc/rollcall
sudo install -m 0644 rollcall-release.pub /etc/rollcall/release.pub
sudo install -m 0755 rollsign-linux-amd64 /opt/rollcall/rollsign
```

装好公钥后**升级流程会变**，先确认这两点再动手：

- 每次升级必须带 `--signature`（签名文件随发布页提供，或自己用 `make sign-release` 生成）；
  忘带会直接失败，`--allow-unsigned` 也会因为"与公钥冲突"被拒绝 —— 这是设计如此
- 每次发版都要同步维护签名：CI 只产二进制与 `SHA256SUMS`，`.sig` 只能在签名机上生成，
  **私钥丢了就只能换一把新的公钥**（旧签名全部失效，需重新分发 `release.pub`）
- **发版时序（v1.21.0 起已由 CI 保障，仍建议遵守）**：先推 tag 等流水线全绿，再由签名机
  `gh release create/upload` 上传本地构建 + `.sig`。签名机构建与 CI 构建可能字节不同
  （Go 版本差异），CI 一旦 `--clobber` 覆盖已签名的二进制，release 上就会出现
  「二进制与签名失配」，升级脚本验签必然失败——v1.20.0–v1.20.1 曾实际发生并于
  2026-10-24 重传修复；CI 现已改为「release 已存在则跳过上传」

```bash
# 从发布页取二进制与签名，两者一起校验
curl -fsSLO .../v1.15.0/rollcall-server-linux-amd64
curl -fsSLO .../v1.15.0/rollcall-server-linux-amd64.sig
sudo sh deploy/rollcall-server-upgrade.sh upgrade \
  --file rollcall-server-linux-amd64 --signature rollcall-server-linux-amd64.sig
```

升级与回滚：

```bash
# --sha256 必须填「发布方给出的」校验和（dist/SHA256SUMS 或 Release 页面），
# 不要在目标机上现算刚下载的那个文件：那样两边永远相等，等于没校验。
sudo sh deploy/rollcall-server-upgrade.sh upgrade \
  --file /tmp/rollcall-server.new \
  --signature /tmp/rollcall-server.new.sig \
  --sha256 <发布记录里的 64 位 hex> \
  --service rollcall-server \
  --health http://127.0.0.1:19080/readyz

sudo sh deploy/rollcall-server-upgrade.sh rollback \
  --service rollcall-server --health http://127.0.0.1:19080/readyz
```

- **`/etc/rollcall/release.pub` 存在时必须验签**：`--signature` 不可省略。签名覆盖版本号、
  SHA-256 与文件大小，验签在备份/替换之前完成，失败时当前版本完全不受影响
- 验签后还会要求「签名中的版本 = 二进制 `-version` = 升级后 `/readyz` 版本」，
  防止签错架构或旧产物；`-version` 对不上会在替换前中止
- `--pubkey` / `--rollsign` 可覆盖默认路径。验签工具只从本机路径加载
  （默认 `/opt/rollcall/rollsign`，其次 PATH 中的 `rollsign`），不从下载源获取
- 尚未配置公钥的实例必须显式传 `--allow-unsigned` 才能只校验 SHA-256（会打印警告）；
  该参数与已有公钥/`--signature` 互斥。**不建议长期使用**，它无法抵御下载源被篡改
- `--sha256` 仍为必填；同时检查 ELF 魔数，拒绝把脚本/损坏文件装成服务端
- **二进制路径默认从 `--service` 单元的 `ExecStart` 推导**，不是写死的 `/opt/rollcall/...`：
  否则在多实例主机上，`--service B` 配默认路径会替换掉 A 的二进制、再重启 B，
  A 悄悄跑在旧文件上而脚本报告成功。显式传 `--bin` 且与单元 ExecStart 不一致时脚本直接拒绝
- 健康检查需要 `curl` 或 `wget`；两者都没有时脚本报错退出，确实要跳过请显式传 `--health ""`
  （不传就是"跳过检查也算升级成功"，正是脚本要避免的那种假成功）
- 当前二进制保存为 `rollcall-server.prev`；最多等待 20 秒，要求 `/readyz`
  返回 200 且版本号与目标二进制一致（防止 systemd 仍拉起旧文件）
- 新版本启动失败时自动换回 `.prev` 并再次重启，脚本返回非 0；回滚恢复的是本机
  此前安装过的文件，不需要重新验签
- 只操作二进制与 systemd 服务，不触碰 unit 文件和数据目录；
  如果新版本已经写入更高 schema，回滚前仍需恢复数据备份
- 执行期间持有 `/run/rollcall-upgrade.lock`，并发调用直接失败；因 `kill -9`/断电遗留的
  锁会按 pid 与进程启动时间自动回收

### Agent 升级

**推荐：用安装脚本的升级模式**（需控制端启用 `-agent-dir`）。只替换二进制，
保留 `/etc/rollcall/agent.json` 与 systemd 单元，不需要节点令牌：

```bash
curl -fsSL https://你的面板/agent/install.sh | sudo sh -s -- --upgrade
# 二进制不在默认位置或自动探测失败时：
curl -fsSL https://你的面板/agent/install.sh | sudo sh -s -- --upgrade /opt/rollcall-agent/rollcall-agent
```

脚本会按架构下载，校验服务端 `/agent/download.sha256` 返回的 SHA-256 与临时路径可执行性，
保留原属主，把旧二进制备份为 `<二进制>.prev`，重启 `rollcall-agent`；若重启后未处于
active 状态会自动回滚并返回非 0。没有 systemd 的容器会重启同一低权限用户的后台进程
（日志 `/var/log/rollcall-agent.log`），启动失败同样自动回滚 `.prev`。

**同机演练（沙箱模式）**：想在已装 Agent 的机器（比如控制端本机）演练接入流程、
又不想碰生产 Agent 时，加 `RC_PREFIX` 前缀——二进制/配置/systemd 单元样例/日志
全部落在该目录下，不创建用户、不碰 systemd，结束时按输出里的清理命令删除即可：

```bash
curl -fsSL https://你的面板/agent/install.sh -o /tmp/install.sh
RC_PREFIX=/tmp/rc-drill sudo -E sh /tmp/install.sh https://你的面板 <node-id> <token>
# 沙箱模式与 --upgrade 互斥（升级对象是系统安装，语义不同）
```

手动回滚：

```bash
sudo rollcall-agent -rollback        # 恢复 .prev 并重启
# 或手动：sudo mv <二进制>.prev <二进制> && sudo systemctl restart rollcall-agent
```

**手动替换**（未配置 `-agent-dir`，或需要同时更新 unit）：

```bash
sudo install -m 0755 rollcall-agent /usr/local/bin/rollcall-agent
sudo install -m 0644 deploy/rollcall-agent.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl restart rollcall-agent
```

Agent 会在每次上报时通过 `X-Agent-Version` 请求头上报版本（兼容旧服务端），
升级后下一轮上报（≤30 秒）面板即显示新版本；低于控制端时节点列表与详情标记「可升级」。
老 Agent 不发送该请求头时版本显示 `—`，不影响在线判定。

分发目录里的二进制不会随控制端自动更新。使用 `-agent-dir` 时，每个版本都要替换：

```bash
sudo install -m 0755 dist/rollcall-agent-linux-{amd64,arm64,arm,386} \
                        /opt/rollcall/agent/
```

### 快照版本与回滚

`state.json` 带 `schemaVersion`（当前为 4；v4 新增续费记录、API 令牌摘要、活动事件、
到期提醒记录与流量周期；v3 起历史聚合存放在独立的 `history.json`），
`history.json` 自带独立版本号（当前为 2；schema 2 为聚合桶增加内存/磁盘绝对用量总和，
v1.7 可直接读取 schema 1 并自动补齐）。旧程序遇到更高版本时会：

1. 把文件改名为 `state.json.newer-<时间>` / `history.json.newer-<时间>`；
2. 拒绝启动并在日志中说明需要升级服务端。

因此**回滚旧二进制前，必须先恢复升级前备份的两份文件**，否则旧程序会拒绝载入新格式数据：

```bash
sudo systemctl stop rollcall-server
sudo cp /var/backups/rollcall/state-<升级前时间>.json /opt/rollcall/data/state.json
sudo cp /var/backups/rollcall/history-<升级前时间>.json /opt/rollcall/data/history.json
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
make linux VERSION=v1.13.1
./rollsign sign -key /root/rollcall-update.key -version v1.13.1 \
  -base-url https://updates.example.com/agent/v1.13.1 \
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

### 面板内加密配置备份（v1.10+）

「设置 → 备份」导出配置级加密备份（格式 v2）：节点台账、服务、规则、通知通道、
状态页、主题、续费记录与令牌摘要，不含指标历史。文件用 PBKDF2-SHA256 + AES-256-GCM
加密，响应同时返回 `size` 与 `sha256`。建议在每次大改配置前导出一份并离线保存。

恢复需要备份密码与显式确认，恢复后会保留节点上报令牌（Agent 无需改配置）：

```bash
TOKEN=$(sudo cat /opt/rollcall/data/admin.token)
# 导出
curl -sS -X POST http://127.0.0.1:19080/api/admin/backup \
  -H "X-Admin-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"password":"<备份密码>"}' -o backup.json
# 恢复（confirm 必须是 true；data 为导出响应中的 base64 字符串）
jq -n --arg pw '<备份密码>' --arg data "$(jq -r .data backup.json)" \
  '{confirm:true, password:$pw, data:$data}' > /tmp/restore.json
curl -sS -X POST http://127.0.0.1:19080/api/admin/restore \
  -H "X-Admin-Token: $TOKEN" -H 'Content-Type: application/json' \
  --data-binary @/tmp/restore.json
rm -f /tmp/restore.json backup.json
```

备份密码至少 8 位且不会被保存；忘记密码只能重新导出。管理令牌、账号哈希与
`session.key` 不在配置备份中，仍应随整目录备份一起保护。

### 自动加密备份调度（v1.14+）

面板内导出需要人点一次；无人值守的定期备份用启动参数开启，与手动导出共用同一套加密与恢复接口：

```bash
# 1) 备份密码（服务用户可读即可；不要放进 Environment=，那样会出现在 systemctl show）
sudo sh -c 'base64 /dev/urandom | head -c 32 > /opt/rollcall/backup.pass'
sudo chown rollcall:rollcall /opt/rollcall/backup.pass
sudo chmod 0400 /opt/rollcall/backup.pass

# 2) 找到承载 ExecStart 的 drop-in（不同部署可以是 private-targets.conf 或 10-*.conf）
systemctl cat rollcall-server | grep -n '^ExecStart=/'

# 3) 在该行行尾追加备份参数：先备份文件，再就地追加
#    ★ 必须先判重：sed 每执行一次都会再追加一遍，参数会重复却不会报错
DROPIN=/etc/systemd/system/rollcall-server.service.d/private-targets.conf
sudo cp -p "$DROPIN" "$DROPIN.bak"
if ! grep -q -- '-backup-password-file' "$DROPIN"; then
  sudo sed -i 's|^\(ExecStart=/opt/rollcall/rollcall-server.*\)$|\1 -backup-password-file /opt/rollcall/backup.pass -backup-interval 24h -backup-keep 14|' "$DROPIN"
fi
grep -c -- '-backup-password-file' "$DROPIN"   # 必须是 1

sudo systemctl daemon-reload && sudo systemctl restart rollcall-server
curl -s -H "X-Admin-Token: $(sudo cat /opt/rollcall/data/admin.token)" \
  http://127.0.0.1:19080/api/admin/backup/auto | grep -o '"enabled":[a-z]*'
```

> 备份参数必须和其余参数写在**同一条** `ExecStart` 上：systemd 只允许一条
> `ExecStart`（多条需要 `ExecStart=` 先清空再重写，写错会起不来）；
> `-backup-dir` 默认 `<data>/backups`，已在 `ReadWritePaths` 内，无需额外授权。

- 参数：`-backup-interval`（1m–30d，默认 24h）、`-backup-keep`（1–365，默认 14）、
  `-backup-dir`（默认 `<data>/backups`）；密码也可用 `ROLLCALL_BACKUP_PASSWORD` 传入，
  但环境变量会出现在 `systemctl show` 与 `/proc/<pid>/environ`，不推荐
- **写后即解密校验**：落盘后立刻用同一密码解密比对，"文件生成了"不等于"将来能恢复"；
  校验失败会记 `lastError` 并保留上一份可用备份
- 启动时首次运行排在 30 秒后；**手动触发会推迟这次排期**，不会产生重复备份（v1.14.1 起）
- 查看状态与立即执行：

```bash
TOKEN=$(sudo cat /opt/rollcall/data/admin.token)
curl -s -H "X-Admin-Token: $TOKEN" http://127.0.0.1:19080/api/admin/backup/auto | jq .
curl -s -X POST -H "X-Admin-Token: $TOKEN" http://127.0.0.1:19080/api/admin/backup/auto/run | jq .
```

状态含 `lastAt / lastFile / lastSize / lastChecksum / nextAt / runs / failures / files[]`，
`backup-status.json` 与备份同目录（0600）。恢复仍走 `/api/admin/restore`，步骤见上一节。

> **密码丢了备份就解不开。** 密码文件与备份放在同一台机器只能防"备份文件单独泄露"，
> 建议复制到密码管理器后删除本机副本，并把 `.rcbk` 同步到异地（`rsync`/对象存储）。
> 主题包（`data/themes/`）与指标历史**不在**配置备份内，需单独同步。

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

# 3) 都不提供：服务端生成 24 位随机密码，写入 data/initial-admin-password.txt（0600）
sudo cat /opt/rollcall/data/initial-admin-password.txt
```

方式 3 的日志只记录密码文件路径，不会把密码写进 journal。首次登录成功后
服务端会自动删除该文件；如果登录前发现文件为空或不存在，请确认服务端是否已经
创建过管理员账号（环境变量只在首次创建时生效）。

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

日常修改无需停服：登录面板后进入「设置 → 管理员账号」：

- 「编辑账号」可修改用户名与邮箱，保存时需输入当前密码；
- 「修改密码」需当前密码 + 新密码（至少 12 位）并二次确认；
- 两项操作都会提升会话版本：当前浏览器自动续签，其他设备与旧 Cookie 立即失效；
- 账号变更串行执行并以 0600 原子写回 `admin-user.json`，文件内仍只有哈希。

忘记密码或无法登录时，仍按上面的 `-reset-admin-password` 流程在服务端重置。

### 两步验证（TOTP，v1.10+）

在「设置 → 管理员账号 → 两步验证」中：

1. 输入当前密码生成密钥，用任意 TOTP 应用（Google Authenticator、1Password、
   Aegis 等）扫描或手动录入；
2. 输入应用生成的 6 位动态码完成启用；
3. 停用时同样需要当前密码与有效动态码。

启用后登录分两步：密码正确但未带动态码时返回 `totpRequired: true`，前端自动显示动态码输入框。
服务端接受 ±1 个 30 秒窗口，抵御轻微时钟漂移。启用/停用都会提升会话版本，
其他设备上的旧会话立即失效。丢失 TOTP 设备时，可停服后备份并编辑 `data/admin-user.json`，删除 `totpSecret`
与 `totpEnabled` 两个字段（保持 0600 与其余字段不变），再启动服务即可恢复单因子登录；
也可使用 `-reset-admin-password` 重置账号。

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

### 范围化 API 令牌（v1.10+）

固定管理令牌之外，可在「设置 → API 令牌」创建有独立 scope 与有效期的 `rct_` 令牌：

- `read`：只读接口；`admin`：全部接口（含写操作）
- 明文只在创建响应中出现一次，服务端仅保存 SHA-256 摘要；列表只显示前缀
- `expiresMinutes` 最短 1 分钟（界面预置 30 分钟起）、上限 10 年；`expiresDays` 仍兼容，
  两者只能二选一（0 表示不过期）；到期后请求返回 401
- `paths` 可把令牌收窄到 `/api` 下的路径前缀（如 `/api/overview`、`/api/nodes/<id>`）；
  按路径段边界匹配，`/api/nodes/n1` 不会放行 `/api/nodes/n10`；留空表示全部
- 使用 `Authorization: Bearer rct_...`；令牌摘要随快照持久化，**重启后仍然有效**
- 怀疑泄露时立即删除对应令牌，删除即时生效

自动化脚本建议使用 `read` 令牌；给 AI 或临时任务可再叠加路径限定和分钟级有效期。
只有确实需要写配置时才创建 `admin` 令牌。
Prometheus 仍使用固定的 `X-Admin-Token`/Basic Auth 路径，两者互不影响。

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

### 管理操作审计（v1.14+）

所有 `/api/admin/*` 与 `/api/export/*` 的**写操作**都会记一条审计（GET 不记）：
动作名（如 `nodes.update`、`tokens.create`、`backup.restore`）、操作者、身份类型
（`session` / `admin-token` / `api-token`）、来源 IP、方法、路径、响应状态码。

- **只记元数据，绝不读取请求体** —— 请求体里可能有密码、令牌与私钥
- 落盘 `<data>/audit.jsonl`（0600，逐行 JSON，追加写），内存里保留最近 1 万条走快速路径；
  **查询覆盖整个保留期**（v1.20.1 起）：内存窗口装满且查询涉及更早记录时自动回源扫描文件，
  冷路径在锁外流式执行，不阻塞写操作
- 保留 90 天，超期由后台自动裁剪（文件上限 5 万条）
- 查询：`GET /api/admin/audit?limit=50&offset=0&actor=&action=&q=&range=7d|30d|90d`，
  返回 `entries` / `total` / `stats`（`count`、`earliest`、`latest`、`retentionDays`）
- 容量与保留策略可看 `GET /api/admin/storage`
- 审计文件**不在**加密配置备份内（它是日志而非配置）；需要长期留存时随整目录备份一起归档

```bash
TOKEN=$(sudo cat /opt/rollcall/data/admin.token)
curl -s -H "X-Admin-Token: $TOKEN" \
  'http://127.0.0.1:19080/api/admin/audit?range=7d&action=tokens.delete' | jq '.total, .entries[0]'
```

> 审计记录的是"谁在什么时候改了什么"，不含旧值与新值。需要追配置差异时，
> 用 `backup` 导出的加密备份做前后对比。

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

**降噪与报告参数（v1.10+）**

| 配置 | 作用 |
|---|---|
| `quietStart` / `quietEnd` | 免打扰起止小时（`-1` 关闭，支持跨午夜）；窗口内仅 critical 与恢复通知立即发送 |
| `digestMins` | 摘要聚合窗口（分钟），0 为逐条发送；critical 与恢复通知不聚合 |
| `titleFormat` / `bodyFormat` | 自定义模板，占位符 `{title}` `{detail}` `{node}` `{rule}` `{value}` `{severity}` `{status}` `{time}` `{link}` |
| `expiryDays` | 资产到期提前提醒天数，`-1` 关闭；到期当天另发一次 |
| `reportDaily` / `reportWeekly` / `reportMonthly` | 运行报告开关 |
| `reportHour` | 报告发送小时（0–23，本地时区） |

- 模板对所有通道（含 Webhook）生效，测试通知也会应用当前模板
- 摘要窗口与免打扰重叠时，任务推迟到两者中较晚的时间；实际发送时间可在通知队列中查看
- 报告内容包括节点在线率、平均资源、活跃告警、服务异常、周期流量与月摊成本
- 到期提醒的已发送记录持久化，重启不会重复提醒同一天的资产

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

### 7.4 从其它探针迁移（批量导入，v1.31.0+）

面板「节点」页 →「⇪ 批量导入」，一行一台：

```
# 名称,服务商,地区,国家,分组（后四项可留空）
香港-01,racknerd,圣何塞,US,prod
东京-02,绿云,东京,JP,prod
```

- 逗号或制表符分隔都行；`#` 开头的行忽略；单次最多 200 台
- **重名会被跳过**（大小写与首尾空格不敏感，批内重复也算），并在结果里列出原因；
  其余行照常创建 —— 迁移时不必先清理重复项
- 导入完成后弹窗给出每台的接入命令（含该节点令牌），复制后逐台粘贴执行即可
- 令牌由服务端生成；导出/备份里能看到它，务必与 `state.json` 同等对待
- 台账里的历史指标不随导入迁移（新节点的曲线从接入那一刻开始长）

迁移顺序建议：先导入建档 → 逐台执行接入命令 → 确认在线后从旧探针下线该节点。
旧的告警规则与通知渠道需要在 Rollcall 里重建一次（两边规则模型不同，不做自动转换）。

### 7.5 TLS 与 Agent 证书指纹（v1.29.0 / v1.30.0+）

控制端启用内置 TLS 后，Agent 默认只做到「加密」：自签证书配 `tlsSkipVerify: true`
意味着**不验证服务端身份**，链路上的主动中间人可以用另一张自签证书接管上报
（上报里有节点令牌）。v1.30.0 起可用证书指纹把信任收敛到「安装时那张证书」。

```bash
# 控制端自己的指纹（公开接口，等价于 openssl 的结果）
curl -kfsSL https://127.0.0.1:19080/agent/tls-pin
openssl x509 -in /etc/rollcall/tls/cert.pem -noout -fingerprint -sha256   # 人工核对用
```

新装节点由 `install.sh` 自动取指纹写入 `agent.json`，无需手工配置：

```json
{
  "server": "https://203.0.113.10:19080",
  "nodeId": "...",
  "token": "...",
  "interval": 30,
  "tlsSkipVerify": true,
  "serverCertPin": "9f2c1b0a…（64 位十六进制）",
  "update": { "enabled": false }
}
```

- **指纹优先于 `tlsSkipVerify`**：配了指纹就按指纹校验，`tlsSkipVerify` 仅作为
  「没有指纹时」的退路
- **指纹写错 = Agent 启动失败**（日志 `TLS 配置无效: 证书指纹应为 32 字节…`）。
  刻意不退化：静默降级会让使用者误以为在校验身份
- 已装节点启用：在 `agent.json` 里补一行 `"serverCertPin": "<指纹>"`，
  `systemctl restart rollcall-agent`，再确认节点在线（改动前先 `cp -p` 备份）
- 升级脚本 `--upgrade` 只提示、不改配置：避免「回滚二进制却带着新配置」的不对等风险

**证书轮换顺序（重要）**：换证书会让所有「已固定指纹」的节点立刻失联，
所以不能「先换证书再逐台更新」。最稳妥的顺序是把连通性放在前面：

1. 逐台删除 `serverCertPin`（退回 `tlsSkipVerify`，链路仍加密）并重启 Agent
2. 更换控制端证书、重启服务端，确认面板可访问
3. 逐台写回新指纹并重启，确认节点在线

10 年自签证书平时不需要走这一步；只有换域名/换 CA 时才涉及。

**边界**：指纹防的是「证书被替换」；首次安装那一刻若链路已被控制，取到的指纹
也会被一起替换（TOFU 固有盲区，面板显示指纹就是为了让人工核对）。
要根治需要公共 CA 证书（上域名后换 Caddy / Let's Encrypt）。

**★ v1.35.1 生产实况（2026-10-26）：已根治。** 机群切换到 Caddy + ACME 架构：

- 生产入口改为 `https://status.endgo.top`（Caddy 2.6.2 监听 80/443，ACME 自动签发续期，
  Let's Encrypt，HTTP-01）；`/etc/caddy/Caddyfile` 仅一条 `reverse_proxy 127.0.0.1:19080`
- 服务端回绑 `127.0.0.1:19080` **明文**（回环明文是合法形态），删 `tls.conf` 与
  `-allow-insecure`；公网不再暴露 19080。回滚：`rm zz-loopback.conf` +
  恢复 `/root/dropin-private-targets.conf.bak` + `daemon-reload` + restart
- **13 台 Agent 全部删掉 `serverCertPin`/`tlsSkipVerify`**，`server` 改为域名，
  靠系统 CA 包真实验证服务端身份（实测 12 台裁剪镜像全部自带 ca-certificates，
  无需兜底；若遇缺包节点，可用 `SSL_CERT_FILE` 环境变量指向 ISRG Root X1 而不装包）
- 切换前每台备份 `agent.json.bak-acme`；IP 直连 `agent.json.http-bak` 仍在
- clientIP/trustedProxies（web.go）默认信任回环，Caddy 同机部署零配置，
  lastIP/限速/审计经反代后仍是真实客户端 IP，已实测
- 面板「对外地址」已设为 `https://status.endgo.top`（落盘于 settings，非启动参数）
- ACME 注意：Caddy 证书 60 天自动续期，**Agent 走 CA 验证后与证书轮换解耦**，
  不再需要「换证书先逐台删指纹」的编排；域名解析变更或 80 端口被封才会影响续期

### 7.7 ICMP 探测的前提（v1.17.0+）

ICMP 探测（ping）默认可用，但依赖内核允许非特权 ping：

```bash
# 应覆盖 rollcall 用户的组（0 2147483647 表示全部放行）
sysctl net.ipv4.ping_group_range
```

- 不覆盖时不会崩，只是 ICMP 探测会在详情里报
  `没有 ICMP 权限：请确认 sysctl net.ipv4.ping_group_range …`
- 放开的办法（把范围设成全部组）：
  `sysctl -w net.ipv4.ping_group_range="0 2147483647"`，并写进 `/etc/sysctl.d/`
- **不要**为此给服务单元加 `CAP_NET_RAW`：非特权路径可用时提权没有收益，
  只会扩大被攻破后的能力面
- 目标只填主机名或 IP，不带协议与端口；只支持 IPv4，IPv6 目标会明确报错
- 与 http/tcp 一样受「默认拒绝内网目标」约束，确有需要时用 `-allow-private-probes`

### 7.6 节点登录告警（v1.19.0+）

Agent 读 `/var/log/wtmp`，只把「自上次上报以来的新登录」随上报带回；服务端去重后
写入节点台账并推送通知（复用「设置 → 通知」里的渠道）。

```bash
# 看某台节点的最近登录
curl -sS -H "X-Admin-Token: $TOKEN" http://127.0.0.1:19080/api/admin/nodes/<id> \
  | grep -o '"lastLogin":[^}]*}'
```

- **只能看到成功登录**：wtmp 不记录失败尝试。防爆破要靠 fail2ban/sshd 日志，不是这里
- **升级顺序：服务端 → Agent**。`Report` 用 `DisallowUnknownFields`，
  先升 Agent 会让旧服务端整份 400、节点全部失联（与上报签名那轮的顺序**相反**）
- 水位是进程内的：Agent 重启后首次上报只记录当前最新时间、不补报历史
- Agent 需要对 `/var/log/wtmp` 有读权限（发行版通常 `0664 root:utmp`，普通用户可读）；
  读不到时跳过，不影响其它指标上报
- **★ 先确认这台机器到底有没有在写 wtmp**。裁剪过的镜像常常缺 `/run/utmp`、
  `pam.d` 也没配 utmp，此时 sshd 不写 wtmp，登录告警永远不会触发：

  ```bash
  ls -l /var/log/wtmp /run/utmp     # /run/utmp 不存在 = 基本可断定没在写
  stat -c '%y' /var/log/wtmp        # 登一次录，再看这个时间有没有变
  ```

  节点详情里的「登录采集」会显示 Agent 实测到的状态（正常 / 未在写入 N 天 / 不可用）。
  要修：`touch /run/utmp && chown root:utmp /run/utmp && chmod 0664 /run/utmp`
  并补一条 tmpfiles（`f /run/utmp 0664 root utmp -`），再让 sshd 重新加载
- 通知级别：root 从网络登录 → `warn`，其余 → `info`；每条登录一条消息（不合并）

### 7.8 上报签名（v1.16.0+）

上报除了令牌还带签名：`X-Node-Ts`（秒）、`X-Node-Nonce`（16 字节随机）、
`X-Node-Sig`。规范：

```
sig = hex(HMAC-SHA256(token, "v1\n" + ts + "\n" + nonce + "\n" + hex(sha256(body))))
```

服务端只接受 `|ts − 服务端时间| <= 90` 的上报，窗口内 nonce 不可重复。

**是否强制由开关决定**，默认关闭：

```bash
TOKEN=$(sudo cat /opt/rollcall/data/admin.token)
# 看现状：还有哪些在线节点没签名（这个名单就是"开启后会失联"的准确名单）
curl -sS -H "X-Admin-Token: $TOKEN" http://127.0.0.1:19080/api/admin/report-security
# 开启强制
curl -sS -X PUT http://127.0.0.1:19080/api/admin/report-security \
  -H "X-Admin-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"requireSignature":true}'
```

- **升级顺序**：Agent → 服务端 → 开强制。新 Agent 发给旧服务端时签名头会被忽略
  （旧服务端不认识这些头），所以先升 Agent 安全；反过来先开强制会让旧 Agent 立刻 401
- 被拒时会写日志 `拒绝上报 node=… ip=…: <原因>`，Agent 侧日志也会带上同一句原因
- 时钟偏差显示在节点详情（>30 秒标黄）。偏差来自 Agent 自报的时间戳，
  只在签名有效时才采信
- 令牌轮换后 Agent 还用旧令牌 → 签名校验失败（`令牌不匹配或请求体被改动`），
  在节点上改 `agent.json` 的 token 即可
- 强制开启后想回退：把 `requireSignature` 置 false 即可，已签名的 Agent 不受影响

### 7.9 运营类通知与流量校正（v1.15.3+）

三类"非告警"事件复用现有通知渠道（在「设置 → 通知」里启用任意渠道后生效）：

| 事件 | 触发条件 | 去重 |
|---|---|---|
| 节点上报地址变更 | 上报来源 IP 与上次不同 | 同节点 10 分钟 |
| 面板登录 | 登录成功 | 不节流（量极小） |
| 多台同时失联 | 同一轮评估里 >=2 台进入离线 | 一轮一条 |

- IP 变更的严重度：跨 /24（首段不同）抬到 critical，同段变化用 warn
- 合并通知**只合并同一轮**：跨轮次各自成条，避免缓慢掉线的机器把几分钟前的告警吞进一条
- 关闭通知即静默这三类事件；它们不进规则列表，也不受「规则静默」影响
  （合并消息在"同一规则"时仍保留 RuleID，可被该规则的静默命中）

流量对账（填一次即可，之后的增量会继续累加）：

```bash
TOKEN=$(sudo cat /opt/rollcall/data/admin.token)
curl -sS -X POST http://127.0.0.1:19080/api/admin/nodes/<id>/traffic-adjust \
  -H "X-Admin-Token: $TOKEN" -H 'Content-Type: application/json' -d '{"usedGb":123.4}'
```

- `usedGb` 与 `usedBytes` 二选一；按节点当前计费口径写入，响应返回 `before` / `after` / `traffic`
- 写 0 表示本周期归零重算；每次校正都记一条 `nodes.traffic-adjust` 审计
- 面板入口：节点详情 → 流量配额 → 「校正本期」

### 8.0 公开页的访问门槛（v1.15.2+；v1.25.0 起默认开启）

> 默认值变更：v1.15.2–v1.24.x 默认关闭，v1.25.0 起**默认开启**——探针装好即有状态页。
> 显式关闭过的配置升级后保持关闭；不带令牌即全网可看（脱敏保证不变：无 IP/价格/到期日/主机指纹），
> 要私享链接就在「状态页」生成访问令牌。开启与换链：

```bash
TOKEN=$(sudo cat /opt/rollcall/data/admin.token)
# 打开 + 设置访问令牌（令牌 16–64 位，字母/数字/-/_）
curl -sS -X PUT http://127.0.0.1:19080/api/admin/status-page \
  -H "X-Admin-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"enabled":true,"token":"替换成随机串"}'
# 轮换令牌（旧链接立即失效）
curl -sS -X POST http://127.0.0.1:19080/api/admin/status-page/token -H "X-Admin-Token: $TOKEN"
# 关闭
curl -sS -X PUT http://127.0.0.1:19080/api/admin/status-page \
  -H "X-Admin-Token: $TOKEN" -H 'Content-Type: application/json' -d '{"enabled":false}'
```

- 未打开时 `/status`、`/api/status*`、`/status.css|js`、`/vendor/*`、`/themes/*` 全部 **404**
- 打开且设了令牌：带 `?t=<令牌>` 首次访问后种 HttpOnly Cookie（30 天），
  页面内的静态资源与 SSE 不再需要查询串；也支持 `X-Status-Token` 与 `Authorization: Bearer`
- **令牌不是账号体系**：它只证明"知道链接"。真要区分访客，请用反向代理的 Basic Auth
- 逐台隐藏：`hiddenNodes` 里的节点不进列表，详情页也 404；公开备注写在 `publicNotes`，
  与台账内部 `Note` 完全分离（内部备注不会出现在公开页，有测试守着）
- 忘记令牌：面板「公开页 → 访问门槛」能看到当前值；也可用上面的 POST 直接换一个新值

### 8.1 公开状态页主题包

主题包是管理员放在数据目录 `themes/` 下的静态资源目录，只影响公开状态页 `/status`。

```bash
# 以默认数据目录 /opt/rollcall/data 为例
sudo mkdir -p /opt/rollcall/data/themes/aurora
sudo tee /opt/rollcall/data/themes/aurora/theme.json >/dev/null <<'EOF'
{"name":"aurora","title":"极光","version":"1.0","author":"ops","description":"深色玻璃"}
EOF
# status.css 与 status.js 至少提供一个，按需添加
sudo cp status.css status.js /opt/rollcall/data/themes/aurora/
sudo chown -R root:root /opt/rollcall/data/themes
```

- 目录名只能是小写字母、数字与连字符（1–32 位）；`name` 若填写必须与目录名一致
- 清单与资源必须通过校验：未知字段、缺失清单、符号链接、超过 1 MiB 的资源都会被拒绝
- 设置页「外观主题 → 公开状态页主题包」选择，或直接调用：
  `PUT /api/admin/theme-packs`，请求体 `{"name":"aurora"}`；`{"name":""}` 恢复内置外观
- 校验命令：

```bash
curl -sI http://127.0.0.1:19080/themes/aurora/status.css   # 200 + Cache-Control: no-cache
curl -s  http://127.0.0.1:19080/status | grep data-theme-pack
```

- 主题包缺失或校验失败时，公开页会自动回退为内置外观；管理端「外观主题」会显示原因
- 主题包**不在加密配置备份内**，迁移或重建实例时需要单独同步 `data/themes/`

> **安全边界**：主题的 `status.js` 与站点同源运行（CSP 仍为 `script-src 'self'`），
> 可以读取公开页能看到的数据并发起同源请求。安装主题等同授予管理员权限，
> 只使用自己编写或可信来源的主题，不要直接解压来路不明的压缩包。

---

### 7.10 时区（v1.33.1+）

日报/周报的发送时刻、免打扰窗口、流量周期的每月起始日、到期提醒的「当天」
全部按**服务端时区**计算，而服务器（尤其容器）默认是 UTC。

```bash
# 设置页会显示当前生效时区，形如 Local (UTC+0) / Asia/Shanghai (UTC+8)
systemctl show rollcall-server -p Environment | tr ' ' '\n' | grep TZ
# 显式指定（推荐写进 drop-in，而不是依赖宿主机）
# /etc/systemd/system/rollcall-server.service.d/timezone.conf
#   [Service]
#   Environment=TZ=Asia/Shanghai
```

也可以给进程传 `-timezone Asia/Shanghai`（非法 IANA 名会**启动即失败**，不会静默回落）。
时区数据库是内嵌的（`time/tzdata`），distroless 镜像里同样可用。

**★ 切换时区会重算流量周期。** 周期起点是「本月 resetDay 00:00（本地时区）」，
时区一变 `Start` 就变，而写入路径把「`Start` 不同」当作新周期 → **本期用量清零**。

已积累用量时要保住数字，按下面顺序做（会停服务约 10 秒）：

```bash
STATE=/opt/rollcall/data/state.json
systemctl stop rollcall-server
cp -p "$STATE" "/root/rollcall-state-pre-tz-$(date -u +%Y%m%d%H%M%S).json.gz"
# 见 docs/scripts/repin-traffic-tz.py：把每个周期的起点按新时区平移（UTC+8 比 UTC 早 8 小时）
python3 docs/scripts/repin-traffic-tz.py "$STATE" -28800
chown rollcall:rollcall "$STATE"; chmod 600 "$STATE"
systemctl daemon-reload && systemctl start rollcall-server
```

平移后核对：`Start` 应等于新的「本月 resetDay 00:00（UTC+8）」，且用量不回退。
**不这么做也能启动**，只是本期用量从 0 重新累计 —— 这不是故障，是周期语义。

### 7.11 通知按规则分发 + 令牌读审计（v1.34.0+）

**规则级渠道选择**：告警规则编辑弹窗里新增「通知渠道（可多选，留空=全部）」，
对应 `AlertRule.Channels`（存的是渠道 ID，不是名字）。

- 留空 = 发送到全部启用渠道（与旧行为一致）
- 只发选中的渠道；被停用的渠道即使被选中也不投递
- **渠道被删/停用后的回退**：若规则指定的渠道全部不可用，改为发送全部启用渠道，
  并在正文追加一行「⚠ …已不存在或已停用，本次改为发送全部渠道」；
  部分不可用时只跳过缺失的，同样在正文列出。**告警不会因为配置漂移而静默消失**
- 渠道 ID 打错在保存时就 400（不会等到告警发出的那一刻才发现）
- 离线告警合并成一条时，只有「同一规则」的合并才沿用该规则的渠道；
  跨规则合并（无法确定发给谁）一律走全部渠道 —— 漏报比多发严重

**令牌读访问审计**：审计页出现的 `api.read` 记录就是它。

- 只记录**作用域令牌**（`Authorization: Bearer rct_...`）发起的 GET/HEAD，
  浏览器会话与匿名请求不记 —— 否则面板每几秒的轮询会把审计刷满
- 同一令牌在同一路由上 **5 分钟内只记一条**（一次 MCP 会话不会刷出几十条）
- 记录主体=令牌名、来源 IP、方法、路径（**不含查询串**：公开页的 `?t=` 就是访问令牌）、
  状态码与耗时
- 令牌台账同时记录 `lastUsedIp`（最近一次使用的来源地址，每分钟最多更新一次）

这让「`rollcall-mcp` 被用来查了什么、从哪台机器查的」变得可回答 —— 在此之前
审计只看写操作，令牌读数据是完全不可见的。

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
| `--upgrade` 提示未找到配置 | 该节点未通过安装脚本安装；先执行一次完整安装，或手动替换二进制 |
| `--upgrade` 下载 404 | 控制端未配置 `-agent-dir`（`/agent/*` 整体 404），改用手动替换并同步分发目录 |
| 升级后版本仍显示 `—` | Agent 尚未上报（最多 30 秒），或旧 Agent 不支持版本头；确认 `/api/nodes` 中 `agentVersion` 字段 |
| 升级后面板仍提示「可升级」 | 上报的 Agent 版本低于控制端 `-version`；确认升级的是 unit 实际使用的二进制路径 |
| 插件通道未出现 | 是否带 `-notify-plugins`；清单/命令是否在目录内、可执行且非全局可写；服务端启动日志会指出具体清单错误 |
| 启动报历史保留期无效 | 保留期必须满足 1m ≤ 5m ≤ 1h、为对应窗口整数倍、单层不超过 10 年与 100 万桶 |
| 端口被占用 | `ss -ltnp \| grep 19080`；确认没有重复启动旧实例 |

健康检查与最小验证：

```bash
curl -fsS http://127.0.0.1:19080/healthz          # ok（只要进程能响应）
curl -fsS http://127.0.0.1:19080/readyz           # ready（存储/磁盘/快照均正常）
curl -fsS http://127.0.0.1:19080/api/status | head # 公开状态数据
curl -fsS http://127.0.0.1:19080/metrics \
  -H "X-Admin-Token: $(sudo cat /opt/rollcall/data/admin.token)" | head
# 重点观察：rollcall_save_failures_total、rollcall_save_duration_seconds、
# rollcall_history_points、rollcall_notify_rejected_total、rollcall_nodes_{stale,offline,no_data}
```

---

## 10. 上线后检查清单

- [ ] `rollcall-server -version` / `rollcall-agent -version` 与发布版本一致，产物的 SHA-256 与发布记录一致
- [ ] `dist/SHA256SUMS` 恰好 12 行（server/agent/rollsign × amd64/arm64/arm/386）；
      少了 `rollsign-linux-*` 就等于发布包里留了一个没人核对的二进制，而它正是签发 Agent 更新清单的工具
- [ ] 如启用服务端发行签名：`dist/rollcall-server-linux-*.sig` 由离线私钥生成，
      控制端 `/etc/rollcall/release.pub` 与 `/opt/rollcall/rollsign` 已就位；
      升级日志中能看到「发行签名校验通过」（临时跳过必须显式 `--allow-unsigned` 并记录原因）
- [ ] 面板节点列表的 Agent 版本与 `rollcall-agent -version` 一致，无意外「可升级」标记
- [ ] 两个 systemd 单元均为 `active (running)` 且已 `enable`；无 systemd 时确认 `runuser` 后台进程与日志
- [ ] `/healthz` 返回 `ok`、`/readyz` 返回 200 且版本号与目标版本一致，`/status` 可匿名访问且不含主机名/IP/价格
- [ ] 首次登录成功后 `data/initial-admin-password.txt` 已自动删除；`/etc/rollcall/server.env` 中不再保留明文密码
- [ ] 目标节点在线，30–60 秒内有新采样
- [ ] 可用用户名与邮箱分别登录面板；错误密码被拒绝；设置页修改账号/密码需当前密码，改后当前浏览器续签、其他设备旧会话失效；刷新与重启服务后登录态保持；退出后回到登录页
- [ ] 管理令牌 API 与 `/metrics` 抓取仍可用，`data/admin.token` 权限为 0600
- [ ] 已配置备份任务，且实际恢复演练过一次
- [ ] 管理员密码已存入密码管理器，非默认/弱口令；`admin-user.json` 中只有哈希
- [ ] 管理令牌、节点令牌、`admin-user.json`、`session.key` 均未进入仓库、日志或截图
- [ ] 一台主机跑多个实例时，确认 `rollcall-server-upgrade.sh` 的 `--service` 与 `--bin`
      指向同一个实例（脚本会用单元 ExecStart 交叉校验并拒绝不一致的组合）
- [ ] 如启用自更新：`update.manifestUrl` 为 HTTPS、公钥正确、二进制目录可写且 systemd 已放行
- [ ] 如启用通知插件：目录与清单非全局可写，逐通道测试通过
- [ ] 如修改主题：管理页与 `/status` 均按预期显示，重置后恢复默认
- [ ] 如启用主题包：`/themes/<name>/status.css` 返回 200 且 `Cache-Control: no-cache`，
      `/status` 带 `data-theme-pack`，删坏主题包后公开页自动回退、管理端有警示

---

## 11. 卸载

卸载前先备份 `data/`（账号、令牌、历史都在其中），并确认没有其他系统依赖该实例的 API。
删除数据目录不可恢复。

> `/etc/rollcall` 同时被控制端（`server.env`、TLS 证书）和被控端（`agent.json`）使用。
> 同机部署时**不要**在卸载其中一个组件时删掉这个目录——里面有另一组件的凭据。
> 只有确认这台机器上不再运行任何 Rollcall 组件时，才执行最后那条 `rm -rf /etc/rollcall`。

```bash
# 控制端
sudo systemctl disable --now rollcall-server
sudo rm -f /etc/systemd/system/rollcall-server.service
sudo rm -rf /etc/systemd/system/rollcall-server.service.d
sudo systemctl daemon-reload
sudo rm -rf /opt/rollcall                 # 确认备份完成后再执行（含 rollsign 验签工具）
sudo rm -f /etc/rollcall/server.env       # 可能仍留有明文初始密码行
sudo rm -f /etc/rollcall/release.pub      # 服务端发行验签公钥（公钥不敏感，但已无用途）
sudo userdel rollcall 2>/dev/null || true
```

```bash
# 被控端（每台节点）
sudo systemctl disable --now rollcall-agent 2>/dev/null || true
# 无 systemd 的容器改为：sudo pkill -f /usr/local/bin/rollcall-agent
sudo rm -f /etc/systemd/system/rollcall-agent.service
sudo rm -rf /etc/systemd/system/rollcall-agent.service.d   # 空目录也要删，否则 daemon-reload 后仍是一个残留单元目录
sudo rm -f /usr/local/bin/rollcall-agent /usr/local/bin/rollcall-agent.prev
sudo rm -rf /opt/rollcall-agent           # 仅启用签名自更新时存在（含旧版本副本）
sudo rm -f /etc/rollcall/agent.json /var/log/rollcall-agent.log
sudo systemctl daemon-reload
sudo userdel rollcall-agent 2>/dev/null || true
```

```bash
# 这台机器上不再有任何 Rollcall 组件时才执行
sudo rm -rf /etc/rollcall                 # 内含 TLS 私钥等敏感文件
```

最后删除反向代理站点、DNS 记录，并在密码管理器/CI 中吊销不再使用的管理令牌。
控制端数据目录里的节点令牌一旦删除就无法再吊销对应 Agent，
如果这些 Agent 还会连到别处，请先在面板里逐个吊销。

# 运维手册

面向已按 README 完成部署的 Rollcall 实例（v1.3+）。覆盖日常备份、升级回滚、令牌轮换、开关配置与故障排查。

---

## 1. 部署布局

| 项目 | 默认位置 | 说明 |
|---|---|---|
| 服务端二进制 | `/opt/rollcall/rollcall-server` | systemd 单元 `rollcall-server.service` |
| 服务端数据 | `/opt/rollcall/data` | `state.json`（快照）、`admin.token`，权限 0600 |
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
make linux VERSION=v1.3.0
# 或直接使用：
./rollcall-server -version
./rollcall-agent -version
```

升级步骤（务必先备份）：

```bash
sudo cp /opt/rollcall/data/state.json \
        /var/backups/rollcall/state-$(date +%F-%H%M).json.gz
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

`state.json` 带 `schemaVersion`（当前为 1）。旧程序遇到更高版本时会：

1. 把文件改名为 `state.json.newer-<时间>`；
2. 拒绝启动并在日志中说明需要升级服务端。

因此**回滚旧二进制前，必须先恢复升级前备份的快照**，否则旧程序会拒绝载入新格式数据：

```bash
sudo systemctl stop rollcall-server
sudo cp /var/backups/rollcall/state-<升级前时间>.json.gz /opt/rollcall/data/state.json
sudo chown rollcall:rollcall /opt/rollcall/data/state.json
sudo chmod 0600 /opt/rollcall/data/state.json
# 换回旧二进制后启动
sudo systemctl start rollcall-server
```

---

## 3. 备份与恢复

`state.json` 通过「写 `.tmp` → fsync → rename」原子替换，因此**运行中复制也是完整快照**（可能是最近一次自动保存，默认 60 秒前）：

```bash
sudo install -d -m 0700 -o root -g root /var/backups/rollcall
sudo cp /opt/rollcall/data/state.json \
        /var/backups/rollcall/state-$(date +%F-%H%M).json.gz
```

需要连管理令牌一起备份时打包整个数据目录，并确保备份文件不进 Git：

```bash
sudo tar -C /opt/rollcall -czf /var/backups/rollcall/data-$(date +%F).tgz data
sudo chmod 0600 /var/backups/rollcall/data-$(date +%F).tgz
```

恢复：

```bash
sudo systemctl stop rollcall-server
sudo cp /var/backups/rollcall/state-<时间>.json.gz /opt/rollcall/data/state.json
sudo chown rollcall:rollcall /opt/rollcall/data/state.json
sudo chmod 0600 /opt/rollcall/data/state.json
sudo systemctl start rollcall-server
```

> 快照损坏时服务端不会以空库启动，而是把文件隔离为 `state.json.corrupt-<时间>`。
> 确认备份可用后再删除隔离文件，否则会丢失全部节点与令牌。

---

## 4. 令牌管理

### 管理令牌

位于 `data/admin.token`（0600，默认 48 位十六进制）。轮换步骤：

```bash
sudo systemctl stop rollcall-server
sudo sh -c 'openssl rand -hex 24 > /opt/rollcall/data/admin.token'
sudo chown rollcall:rollcall /opt/rollcall/data/admin.token
sudo chmod 0600 /opt/rollcall/data/admin.token
sudo systemctl start rollcall-server
```

轮换后需同步更新浏览器端令牌、Prometheus `basic_auth` 密码及所有脚本。**不要把令牌提交到仓库或贴进聊天记录。**

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

## 7. 告警规则定向与通知并发

- 规则支持 `nodeIds` 与 `groups`（界面在规则弹窗中配置），空表示对所有节点生效；两者同时存在时按「或」匹配。
- 规则收紧或改向后，原先命中但已不在范围内的活跃告警会自动关闭。
- 默认规则只在全新实例上播种一次；删光规则后重启不会复活（持久化标记 `rulesInitialized`）。
- 通知投递并发上限为 **8**。告警风暴时超出的通知会被丢弃，并在服务端日志记录
  `通知并发已达上限 8，丢弃一条 ...`。若频繁出现，说明通道响应慢或告警过于密集，应先修通道而不是调大并发。

---

## 8. 故障排查

| 现象 | 排查方向 |
|---|---|
| 浏览器一直要求令牌 / 401 | 管理接口需 `X-Admin-Token`；节点上报需 `X-Node-Token`，两者不可混用 |
| 节点显示离线 | 服务端 120 秒未收到上报；检查 Agent 日志、时间同步、`interval ≤ 60` |
| Agent 拒绝启动（HTTP） | 非本机服务端默认必须 HTTPS；临时接受风险可加 `-allow-insecure` |
| Agent 读取配置失败 | 确认 `/etc/rollcall` 属组 `rollcall-agent`(0750)、`agent.json` 为 `root:rollcall-agent 0640` |
| 服务探测全部失败 | 目标是否为私网？需显式 `-allow-private-probes`；自签名证书需 `-insecure-probe-tls` |
| 通知发不出去 | 设置页逐通道测试；自建内网服务需 `-allow-private-notify`；查看日志中的丢弃提示 |
| 启动报快照错误 | 查看日志中的 `.corrupt-*` / `.newer-*` 备份路径；修复或恢复备份后再启动 |
| 端口被占用 | `ss -ltnp \| grep 19080`；确认没有重复启动旧实例 |

健康检查与最小验证：

```bash
curl -fsS http://127.0.0.1:19080/healthz          # ok
curl -fsS http://127.0.0.1:19080/api/status | head # 公开状态数据
curl -fsS http://127.0.0.1:19080/metrics \
  -H "X-Admin-Token: $(sudo cat /opt/rollcall/data/admin.token)" | head
```

---

## 9. 上线后检查清单

- [ ] `rollcall-server -version` / `rollcall-agent -version` 与发布版本一致
- [ ] 两个 systemd 单元均为 `active (running)` 且已 `enable`
- [ ] `/healthz` 返回 `ok`，`/status` 可匿名访问且不含主机名/IP/价格
- [ ] 目标节点在线，30–60 秒内有新采样
- [ ] 管理接口无令牌返回 401，带令牌返回 200
- [ ] 已配置备份任务，且实际恢复演练过一次
- [ ] 管理令牌与节点令牌均未进入仓库、日志或截图

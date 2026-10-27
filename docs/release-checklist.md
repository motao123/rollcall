# 发版验收清单（Release DoD）

> 目的：把每次发版实际执行过的步骤固化成可勾选清单，避免「靠记忆」和漏项。
> 用法：复制本文件为 `docs/verify-vX.Y.Z.md`，逐项填入证据（命令输出/截图路径）。
> 版本示例按 v1.37.x 的流程编写；只有一处部署地址，多实例部署请自行替换。

---

## 0. 提交前自检（本地，缺一项就不要打 tag）

| # | 检查 | 命令 | 期望 |
|---|---|---|---|
| 0.1 | Go 格式 | `gofmt -s -l .` | **无输出**（有输出先 `gofmt -s -w <文件>`） |
| 0.2 | 静态检查 | `GOTOOLCHAIN=go1.27.3 go vet ./...` | 无输出 |
| 0.3 | 单测 | `GOTOOLCHAIN=go1.27.3 go test -count=1 ./...` | 全部 `ok`，0 个 FAIL |
| 0.4 | 前端语法 | `node --check internal/web/dist/app.js`（及 `status.js`/`ep-shim.js`） | 无输出 |
| 0.5 | 配色对比度 | `node tools/check-contrast.mjs` | `fail=0`（warn 可为 0） |
| 0.6 | 中文未损坏 | `python -c "b=open('internal/web/dist/app.js','rb').read();print(b.count(b'\xef\xbf\xbd'))"` | 输出 `0`（U+FFFD 计数） |
| 0.7 | dist 行尾 | `python -c "b=open('internal/web/dist/app.js','rb').read();print(b.count(b'\r\n'))"` | 输出 `0`（CRLF 会改变 go:embed 产物） |

> 0.5–0.7 对应三类真实事故：配色不达标、heredoc 写坏中文、CRLF 导致 digest 漂移。

## 1. 版本与文档

| # | 检查 | 期望 |
|---|---|---|
| 1.1 | `README.md` 顶部版本行已改 | `当前版本 **vX.Y.Z**` |
| 1.2 | `## 十三、实际部署验证` 下新增版本条目 | 含：问题 / 根因 / 修复 / 边界说明 |
| 1.3 | 文档改动用**编辑工具**完成 | 未经 `python -c` 拼接（反引号会被 shell 执行） |

## 2. 提交与 CI

| # | 检查 | 命令 | 期望 |
|---|---|---|---|
| 2.1 | 工作树干净 | `git status --short` | 只有预期改动 |
| 2.2 | 提交 | `git commit -m "..."` | 提交信息含版本号与根因一句话 |
| 2.3 | 打 tag + 推送 | `git tag vX.Y.Z && git push origin main vX.Y.Z` | 两条推送都成功 |
| 2.4 | main 流水线 | `gh run list --limit 2` | `success` |
| 2.5 | tag 流水线 | 同上 | `success`，**release 出现 17 个资产**（16 二进制 + SHA256SUMS） |

> tag 流水线偶发 `exit 143`（race job 被 SIGTERM）：`gh run rerun <id> --failed` 即可，不必改代码。

## 3. 构建一致性与签名

| # | 检查 | 命令 | 期望 |
|---|---|---|---|
| 3.1 | 本地四架构构建 | 见下方命令块 | 无报错 |
| 3.2 | digest 对比 | 本地 `sha256sum` vs `gh api ... releases/tags/vX.Y.Z --jq '.assets[].digest'` | **8/8 完全一致** |
| 3.3 | 签名 | `rollsign sign-release -key ... -version vX.Y.Z -file <bin>` × 8 | 8 个 `.sig` 生成 |
| 3.4 | 上传签名 | `gh release upload vX.Y.Z <8 个 .sig> --clobber` | 上传成功 |

```bash
# 3.1 本地构建（必须钉工具链，否则与 CI 产物不同）
for arch in amd64 arm64 arm 386; do for bin in server agent; do
  GOSUMDB=sum.golang.org GOTOOLCHAIN=go1.27.3 GOOS=linux GOARCH=$arch CGO_ENABLED=0 \
  go build -trimpath -buildvcs=false \
    -ldflags "-s -w -X 'github.com/motao123/rollcall/internal/version.Version=vX.Y.Z'" \
    -o build-vX.Y.Z/rollcall-$bin-linux-$arch ./cmd/$bin
done; done
```

> digest 不一致时**先查行尾**（`\r\n` 计数），不要怀疑编译器：go:embed 嵌的是原始字节。

## 4. 生产部署（服务端）

| # | 检查 | 期望 |
|---|---|---|
| 4.1 | 下载并校验 | `rollcall-server-linux-amd64: OK` |
| 4.2 | **端到端验签**（在生产机执行） | 输出 `vX.Y.Z` |
| 4.3 | 版本断言 | `rollcall-server vX.Y.Z` |
| 4.4 | 服务状态 | `active` |
| 4.5 | 健康检查 | `readyz: HTTP 200` |
| 4.6 | 旧版保留 | `/opt/rollcall/rollcall-server.<上一版>.bak` 存在 |

部署脚本：`scripts/deploy_server_vXXXX.sh`（由上一版 sed 版本号生成，勿手写）。

## 5. Agent 分发与升级

| # | 检查 | 期望 |
|---|---|---|
| 5.1 | 分发目录刷新 | `/opt/rollcall/agent/rollcall-agent-linux-{amd64,arm64,arm,386}` 四架构 `OK` |
| 5.2 | `SHA256SUMS` 同步 | `grep agent SHA256SUMS \| sha256sum -c -` 全 OK |
| 5.3 | 常规节点升级 | 脚本逐台输出 `成功` |
| 5.4 | 上海（低带宽） | SFTP 直传，`UPGRADE_OK` |
| 5.5 | 成都 | SFTP 直传，`UPGRADE_OK` |
| 5.6 | 绿云-jp（SSH 常不通） | 经控制端隧道 `-L 12222:...:22`，`UPGRADE_OK` |
| 5.7 | 面板本机 | `/agent/download` + sha256 断言，`active` |
| 5.8 | 版本一致性 | `/api/admin/nodes` 全部为 `vX.Y.Z`，**`agentOutdated` 为空** |

> 升级脚本里的 `EXPECTED` **必须从 Release API 或 `/agent/download.sha256` 动态取**，
> 不要 sed 版本号（sed 不会改 64 位 hash，曾因此三次误判 SHA_MISMATCH）。

## 6. 上线后回归

| # | 检查 | 命令 | 期望 |
|---|---|---|---|
| 6.1 | UI 不变量 | `RCP_PASSWORD=... node tools/ui-check.mjs` | 全绿（当前 13 项） |
| 6.2 | 安全头与证书 | `node tools/audit-headers.mjs` | 12/12 通过，证书剩余 ≥14 天 |
| 6.3 | 对比度 | `node tools/check-contrast.mjs` | `fail=0` |
| 6.4 | 公开页 | `curl -s -o /dev/null -w '%{http_code}' https://<域名>/status` | 200 |
| 6.5 | 本版新功能 | 按条目里的断言逐项实测（CDP 截图留证） | 与条目描述一致 |

## 7. 收尾

| # | 检查 | 期望 |
|---|---|---|
| 7.1 | 清理构建产物 | `build-vX.Y.Z/` 已删 |
| 7.2 | 清理临时文件 | `/tmp` 下的 `r*.txt` / `v.py` / 密码副本已删 |
| 7.3 | 工作树干净 | `git status --short` 无输出 |
| 7.4 | 记忆更新 | `.workbuddy/memory/YYYY-MM-DD.md` 记录：版本、根因、新踩的坑 |
| 7.5 | 敏感文件 | 仓库内无 `rcpwd.txt` / `*.key` / `admin.token`（`.auth/` 已 gitignore） |

---

## 附：常见失败处置

| 现象 | 原因 | 处置 |
|---|---|---|
| CI `gofmt` 红 | 提交前没跑 `gofmt -s -w` | 格式化后补提交，重打 tag |
| 8/8 digest 不一致 | dist 被写入 CRLF | 二进制归一 LF 后重建 |
| 生产验签失败 | `.sig` 未上传或版本号不符 | 重签重传 |
| Agent 升级 `SHA_MISMATCH` | 脚本里 EXPECTED 是旧 hash | 从 Release API 动态取 |
| `Text file busy` | 同机有另一路升级在写同一临时文件 | 停掉另一路，`pkill -f rollcall-agent-new` |
| 隧道被拒 | `A && B & C` 让整条链后台化 | 改为 `ssh -N -L ... & TP=$!; sleep 5; ...; kill $TP` |

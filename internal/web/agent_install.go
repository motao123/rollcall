package web

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// 一条命令接入：服务端直接分发 Agent 二进制并生成安装脚本。
//
// 目的是把「拷贝二进制 + 手写 unit + 拼 JSON」这几步压成一行，
// 同时保证生成出来的配置与当前控制端地址一致（避免手工填错 server 或漏 -allow-insecure）。
// 脚本本身不含任何密钥：node id 与 token 由调用方作为参数传入，
// 因此可以公开下发，也能被审计（不存在「脚本里藏着别人的令牌」这类问题）。

// agentInstallScript 是 /agent/install.sh 的内容。
// 安装参数顺序固定为 <server> <node-id> <token>；
// --upgrade 模式只替换二进制，复用已有配置，不再需要节点令牌。
const agentInstallScript = `#!/bin/sh
# Rollcall Agent 安装/升级脚本（由控制端生成）
# 安装: curl -fsSL %[1]s/agent/install.sh | sh -s -- %[1]s <node-id> <token>
# 升级: curl -fsSL %[1]s/agent/install.sh | sh -s -- --upgrade [二进制路径]
set -eu

usage() {
    echo "用法:" >&2
    echo "  安装: sh install.sh <server> <node-id> <token>" >&2
    echo "  升级: sh install.sh --upgrade [二进制路径]" >&2
    exit 2
}

if [ "$(id -u)" != "0" ]; then
    echo "请用 root 运行（例如 curl ... | sudo sh -s -- ...）" >&2
    exit 1
fi

UPGRADE=0
BIN_ARG=""
if [ "${1:-}" = "--upgrade" ] || [ "${1:-}" = "-u" ]; then
    UPGRADE=1
    BIN_ARG=${2:-}
    [ "$#" -le 2 ] || usage
else
    [ "$#" -ge 3 ] || usage
    [ "$#" -le 3 ] || usage
fi

CONFIG=/etc/rollcall/agent.json

if [ "$UPGRADE" = "1" ]; then
    if [ ! -f "$CONFIG" ]; then
        echo "未找到 $CONFIG：该节点尚未安装 Agent，请先执行安装命令" >&2
        exit 1
    fi
    # 升级不需要节点令牌：沿用现有配置里的控制端地址，避免指错实例。
    SERVER=$(sed -n 's/.*"server"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$CONFIG" | head -n 1)
    if [ -z "$SERVER" ]; then
        echo "无法从 $CONFIG 读取 server，请检查配置格式" >&2
        exit 1
    fi
    NODE_ID=""
    TOKEN=""
else
    SERVER=$1
    NODE_ID=$2
    TOKEN=$3
fi

SERVER=$(printf '%%s' "$SERVER" | sed 's:/*$::')

case "$(uname -s)" in
    Linux) ;;
    *) echo "仅支持 Linux（当前 $(uname -s)）" >&2; exit 1 ;;
esac

case "$(uname -m)" in
    x86_64|amd64) ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    armv7l|armv6l|arm) ARCH=arm ;;
    i386|i686) ARCH=386 ;;
    *) echo "不支持的架构: $(uname -m)" >&2; exit 1 ;;
esac

# 明文 HTTP 且非回环：Agent 默认拒绝上报，必须显式放行。
case "$SERVER" in
    http://127.0.0.1*|http://localhost*|http://\[::1\]*) INSECURE="" ;;
    http://*) INSECURE=" -allow-insecure" ;;
    *) INSECURE="" ;;
esac

if [ "$UPGRADE" = "1" ]; then
    BIN=$BIN_ARG
    if [ -z "$BIN" ] && command -v systemctl >/dev/null 2>&1; then
        # 自更新用户可能把二进制放在 /opt，从运行单元里取真实路径。
        BIN=$(systemctl show -p ExecStart --value rollcall-agent 2>/dev/null \
            | sed -n 's/.*path=\([^ ;]*\).*/\1/p' | head -n 1)
    fi
    [ -n "$BIN" ] || BIN=/usr/local/bin/rollcall-agent
    if [ ! -x "$BIN" ]; then
        echo "未找到可执行的 Agent: $BIN（可用 --upgrade <路径> 指定）" >&2
        exit 1
    fi
else
    BIN=/usr/local/bin/rollcall-agent
fi

BIN_DIR=$(dirname "$BIN")
if [ ! -w "$BIN_DIR" ]; then
    echo "目录不可写: $BIN_DIR" >&2
    exit 1
fi

TMP="$BIN.new.$$"
echo "→ 下载 Agent ($ARCH)"
if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$SERVER/agent/download?arch=$ARCH" -o "$TMP"
elif command -v wget >/dev/null 2>&1; then
    wget -q -O "$TMP" "$SERVER/agent/download?arch=$ARCH"
else
    echo "需要 curl 或 wget" >&2; exit 1
fi
chmod 0755 "$TMP"

# 先在临时路径上验证能执行，避免把半个文件装成正式二进制
if ! "$TMP" -version >/dev/null 2>&1; then
    echo "下载的二进制无法执行，已放弃安装" >&2
    rm -f "$TMP"
    exit 1
fi

if [ "$UPGRADE" = "1" ]; then
    OLD_VER=$("$BIN" -version 2>/dev/null || echo unknown)
    # 保留原属主：自更新目录通常归 rollcall-agent，换成 root 会让下次自更新失效。
    OWNER=$(stat -c '%%u:%%g' "$BIN" 2>/dev/null || true)
    if [ -n "$OWNER" ]; then
        chown "$OWNER" "$TMP" 2>/dev/null || true
    fi
    cp -p "$BIN" "$BIN.prev"
    mv -f "$TMP" "$BIN"
    NEW_VER=$("$BIN" -version 2>/dev/null || echo unknown)
    echo "✔ Agent 已替换: $OLD_VER → $NEW_VER"
    echo "  配置未改动；旧版本备份在 $BIN.prev"

    if command -v systemctl >/dev/null 2>&1 && systemctl cat rollcall-agent.service >/dev/null 2>&1; then
        # set -e 下必须吞掉 restart 的失败，否则会跳过下面的回滚分支。
        systemctl restart rollcall-agent || true
        sleep 2
        if systemctl is-active --quiet rollcall-agent; then
            echo "✔ rollcall-agent 已重启，约 30 秒内可在面板看到新版本"
        else
            echo "✘ rollcall-agent 重启失败，正在回滚…" >&2
            mv -f "$BIN.prev" "$BIN"
            systemctl restart rollcall-agent || true
            sleep 2
            if systemctl is-active --quiet rollcall-agent; then
                echo "✔ 已回滚到旧版本" >&2
            else
                echo "✘ 回滚后仍未启动，请查看 journalctl -u rollcall-agent" >&2
                systemctl status rollcall-agent --no-pager >&2 || true
            fi
            exit 1
        fi
    else
        echo "⚠ 未检测到 rollcall-agent.service，请手动重启 Agent 使新版本生效"
    fi
    exit 0
fi

mv -f "$TMP" "$BIN"

# 专用低权限用户：Agent 只需读 /proc，不需要 root。
# ★ 必须与 deploy/rollcall-agent.service 保持一致：否则"一条命令接入"装出来的
#   Agent 以 root 运行，而同仓库的模板是低权限 —— 两种接入方式的安全姿态不同，
#   用户以为自己在跑最小权限，实际拿到的是 root 常驻进程。
AGENT_USER=rollcall-agent
AS_USER=""
if id -u "$AGENT_USER" >/dev/null 2>&1; then
    AS_USER=$AGENT_USER
elif command -v useradd >/dev/null 2>&1; then
    # useradd 在某些容器里存在但被限制；失败就退回 root 运行而不是中断安装。
    useradd --system --no-create-home --shell /usr/sbin/nologin "$AGENT_USER" 2>/dev/null || true
    id -u "$AGENT_USER" >/dev/null 2>&1 && AS_USER=$AGENT_USER
fi

if [ -n "$AS_USER" ]; then
    SVC_USER="User=$AS_USER
Group=$AS_USER"
    install -d -m 0750 -o root -g "$AS_USER" /etc/rollcall
else
    echo "⚠ 未能创建低权限用户 $AGENT_USER（可能缺少 useradd），Agent 将以 root 运行" >&2
    SVC_USER=""
    install -d -m 0700 /etc/rollcall
fi

umask 077
cat > /etc/rollcall/agent.json <<JSON
{
  "server": "$SERVER",
  "nodeId": "$NODE_ID",
  "token": "$TOKEN",
  "interval": 30,
  "update": { "enabled": false }
}
JSON
if [ -n "$AS_USER" ]; then
    chown root:"$AS_USER" /etc/rollcall/agent.json
    chmod 0640 /etc/rollcall/agent.json
else
    chmod 0600 /etc/rollcall/agent.json
fi

cat > /etc/systemd/system/rollcall-agent.service <<UNIT
[Unit]
Description=Rollcall Agent (fleet monitoring)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
$SVC_USER
ExecStart=$BIN -config /etc/rollcall/agent.json$INSECURE
Restart=always
RestartSec=10s
ReadOnlyPaths=/proc

# 采集器只读：无特权、只读文件系统、禁写可执行内存
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
MemoryDenyWriteExecute=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LockPersonality=true

[Install]
WantedBy=multi-user.target
UNIT

# 容器/LXC 里常没有可用的 systemd：退回直接后台运行，
# 否则整条安装会因为 systemctl 不存在而失败。
if ! command -v systemctl >/dev/null 2>&1; then
    echo "→ 未检测到 systemd，改为后台常驻"
    nohup "$BIN" -config /etc/rollcall/agent.json$INSECURE >/var/log/rollcall-agent.log 2>&1 &
    sleep 2
    if pgrep -f "$BIN" >/dev/null 2>&1; then
        echo "✔ Agent 已在后台运行，约 30 秒内可在面板看到该节点"
    else
        echo "✘ Agent 启动失败，查看 /var/log/rollcall-agent.log" >&2
        exit 1
    fi
    exit 0
fi

systemctl daemon-reload
systemctl enable rollcall-agent >/dev/null 2>&1 || true
systemctl restart rollcall-agent || true
sleep 2

if systemctl is-active --quiet rollcall-agent; then
    echo "✔ Agent 已启动，约 30 秒内可在面板看到该节点"
else
    echo "✘ Agent 启动失败，排查：" >&2
    systemctl status rollcall-agent --no-pager >&2 || true
    journalctl -u rollcall-agent -n 30 --no-pager >&2 || true
    exit 1
fi
`

// handleAgentInstall 返回安装脚本。公开可访问：脚本本身不含密钥。
func (s *Server) handleAgentInstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	base := requestBaseURL(r)
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	fmt.Fprintf(w, agentInstallScript, base)
}

// handleAgentDownload 下发对应架构的 Agent 二进制。
// 目录由 -agent-dir 指定；未配置时明确报错，而不是给一个空文件。
func (s *Server) handleAgentDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	arch := r.URL.Query().Get("arch")
	switch arch {
	case "amd64", "arm64", "arm", "386":
	default:
		fail(w, http.StatusBadRequest, "arch 必须是 amd64/arm64/arm/386")
		return
	}
	if s.agentDir == "" {
		fail(w, http.StatusNotFound, "服务端未启用 Agent 分发（启动参数 -agent-dir 未配置）")
		return
	}
	// 白名单校验 arch 后再拼路径，杜绝 .. 之类的穿越。
	name := "rollcall-agent-linux-" + arch
	p := filepath.Join(s.agentDir, name)
	f, err := os.Open(p)
	if err != nil {
		fail(w, http.StatusNotFound, "该架构的 Agent 尚未放到分发目录: "+name)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		fail(w, http.StatusNotFound, "Agent 文件不可用")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, name, fi.ModTime(), f)
}

// requestBaseURL 推断控制端的外部地址（scheme + host）。
// 反代场景优先信 X-Forwarded-Proto，否则按是否 TLS 判断。
func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if v := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); v == "http" || v == "https" {
		scheme = v
	}
	host := r.Host
	if host == "" {
		host = "127.0.0.1:19080"
	}
	return scheme + "://" + host
}

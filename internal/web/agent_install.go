package web

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
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
# 安装: curl -fsSL %[1]s/agent/install.sh | sh -s -- [--allow-insecure] %[1]s <node-id> <token>
# 升级: curl -fsSL %[1]s/agent/install.sh | sh -s -- [--allow-insecure] --upgrade [二进制路径]
# --allow-insecure 仅用于控制端是明文 HTTP 且链路可信的场景：下载的二进制会以 root 执行。
set -eu

usage() {
    echo "用法:" >&2
    echo "  安装: sh install.sh [--allow-insecure] <server> <node-id> <token>" >&2
    echo "  升级: sh install.sh [--allow-insecure] --upgrade [二进制路径]" >&2
    echo "  沙箱演练（不影响系统与已装 Agent）: RC_PREFIX=/abs/path sh install.sh ... <server> <node-id> <token>" >&2
    exit 2
}

if [ "$(id -u)" != "0" ]; then
    echo "请用 root 运行（例如 curl ... | sudo sh -s -- ...）" >&2
    exit 1
fi

fetch_file() {
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$1" -o "$2"
    elif command -v wget >/dev/null 2>&1; then
        wget -q -O "$2" "$1"
    else
        echo "需要 curl 或 wget" >&2
        return 1
    fi
}

fetch_text() {
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$1"
    elif command -v wget >/dev/null 2>&1; then
        wget -q -O - "$1"
    else
        echo "需要 curl 或 wget" >&2
        return 1
    fi
}

ALLOW_INSECURE=0
while [ "$#" -gt 0 ]; do
    case "${1:-}" in
        --allow-insecure) ALLOW_INSECURE=1; shift ;;
        *) break ;;
    esac
done

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
ETC_DIR=/etc/rollcall
UNIT_FILE=/etc/systemd/system/rollcall-agent.service
LOG_FILE=/var/log/rollcall-agent.log
# 沙箱演练：RC_PREFIX 指向绝对路径时，二进制/配置/单元样例/日志全部落在
# 前缀下，不创建用户、不碰 systemd、不影响已安装的 Agent——用于在控制端
# 本机（或任何已有一台 Agent 的机器）安全地演练接入流程。
SANDBOX=0
case "${RC_PREFIX:-}" in
    "") ;;
    /*)
        SANDBOX=1
        mkdir -p -- "$RC_PREFIX" || { echo "无法创建 RC_PREFIX: $RC_PREFIX" >&2; exit 1; }
        ETC_DIR=$RC_PREFIX/etc/rollcall
        CONFIG=$ETC_DIR/agent.json
        UNIT_FILE=$RC_PREFIX/etc/systemd/system/rollcall-agent.service
        LOG_FILE=$RC_PREFIX/var/log/rollcall-agent.log
        echo "◎ 沙箱模式（RC_PREFIX=$RC_PREFIX）：所有文件落在该前缀下，不改动系统配置" >&2
        ;;
    *) echo "RC_PREFIX 必须是以 / 开头的绝对路径" >&2; exit 1 ;;
esac

if [ "$UPGRADE" = "1" ]; then
    if [ "$SANDBOX" = "1" ]; then
        echo "沙箱模式不支持 --upgrade（升级对象是系统安装的 Agent）" >&2
        exit 2
    fi
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
# 地址直接写进配置文件，必须是完整 URL。
case "$SERVER" in
    http://?*|https://?*) ;;
    *) echo "控制端地址必须以 http:// 或 https:// 开头: $SERVER" >&2; exit 1 ;;
esac
# 参数会进入 JSON 配置：拒绝控制字符，并转义反斜杠与引号。
if printf '%%s' "$SERVER$NODE_ID$TOKEN" | LC_ALL=C grep -q '[[:cntrl:]]'; then
    echo "参数包含控制字符，拒绝写入配置" >&2
    exit 1
fi
json_escape() {
    printf '%%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'
}
SERVER_JSON=$(json_escape "$SERVER")
NODE_ID_JSON=$(json_escape "$NODE_ID")
TOKEN_JSON=$(json_escape "$TOKEN")

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

# 明文 HTTP 且非回环：下载后立即以 root 执行，必须显式确认链路可信。
# 不能只打印警告就继续：复制错地址或误用公网 http 时，攻击者可在链路上
# 替换二进制（脚本本身也经同一明文通道下发）；--allow-insecure 让风险变成明确选择。
case "$SERVER" in
    # 精确匹配主机名，不能用 127.0.0.1* 这样的前缀：
    # http://127.0.0.1.evil.com 与 http://127.0.0.1@evil.com 都会命中前缀，
    # 却把二进制下载指向外部主机（脚本随后以 root 执行它）。
    http://127.0.0.1|http://127.0.0.1/*|http://127.0.0.1:*) INSECURE="" ;;
    http://localhost|http://localhost/*|http://localhost:*) INSECURE="" ;;
    http://\[::1\]|http://\[::1\]/*|http://\[::1\]:*) INSECURE="" ;;
    http://*)
        if [ "$ALLOW_INSECURE" != "1" ]; then
            echo "控制端使用明文 HTTP：下载的 Agent 会以 root 身份执行，链路可被窃听/篡改。" >&2
            echo "确认网络链路可信后，在命令中加 --allow-insecure 重新执行；长期使用请启用 HTTPS。" >&2
            exit 1
        fi
        INSECURE=" -allow-insecure"
        echo "⚠ 控制端使用明文 HTTP（已通过 --allow-insecure 放行）：二进制下载与控制通道都可被窃听/篡改，建议尽快启用 HTTPS" >&2
        ;;
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
    BIN=${RC_PREFIX:+$RC_PREFIX/}/usr/local/bin/rollcall-agent
fi

BIN_DIR=$(dirname "$BIN")
if [ "$SANDBOX" = "1" ]; then
    mkdir -p -- "$BIN_DIR"
fi
if [ ! -w "$BIN_DIR" ]; then
    echo "目录不可写: $BIN_DIR" >&2
    exit 1
fi

# agent_pids 只匹配 /proc/<pid>/exe 精确指向 $BIN 的进程。
# 不能用 pgrep/pkill 的模糊匹配：任何命令行里包含该路径的进程
# （编辑器、tail、另一次安装）都会被误杀。替换二进制后旧进程的
# exe 链接会带 " (deleted)" 后缀，这里一并归一化。
agent_pids() {
    target=$(readlink -f "$BIN" 2>/dev/null || printf '%%s' "$BIN")
    for exe in /proc/[0-9]*/exe; do
        [ -e "$exe" ] || continue
        link=$(readlink "$exe" 2>/dev/null) || continue
        link=${link%% (deleted)}
        [ "$link" = "$target" ] || continue
        pid=${exe#/proc/}
        printf '%%s\n' "${pid%%/exe}"
    done
}

stop_agent() {
    pids=$(agent_pids)
    [ -n "$pids" ] || return 0
    echo "→ 停止已在运行的 Agent: $(printf '%%s ' $pids)"
    kill $pids 2>/dev/null || true
    n=0
    while [ -n "$(agent_pids)" ] && [ "$n" -lt 5 ]; do
        sleep 1
        n=$((n + 1))
    done
    pids=$(agent_pids)
    if [ -n "$pids" ]; then
        echo "⚠ Agent 未在 5 秒内退出，强制结束" >&2
        kill -9 $pids 2>/dev/null || true
        sleep 1
    fi
}

agent_run_user() {
    for pid in $(agent_pids); do
        u=$(ps -o user= -p "$pid" 2>/dev/null | tr -d ' ')
        if [ -n "$u" ] && [ "$u" != "root" ]; then
            printf '%%s' "$u"
            return 0
        fi
    done
    return 0
}

# 进程已停止时从配置属组推断原运行用户（低权限安装为 root:rollcall-agent 0640）。
agent_user_from_config() {
    [ -f "$CONFIG" ] || return 0
    grp=$(stat -c '%%G' "$CONFIG" 2>/dev/null || true)
    case "$grp" in
        ""|root|UNKNOWN) return 0 ;;
    esac
    if id -u "$grp" >/dev/null 2>&1; then
        printf '%%s' "$grp"
    fi
}

start_agent_bg() {
    LOG=$LOG_FILE
    : >> "$LOG"
    if [ -n "${RUN_AS:-}" ] && [ "$RUN_AS" != "root" ] \
        && command -v runuser >/dev/null 2>&1 && id -u "$RUN_AS" >/dev/null 2>&1; then
        chown "$RUN_AS:$RUN_AS" "$LOG" 2>/dev/null || chown "$RUN_AS" "$LOG" 2>/dev/null || true
        chmod 0640 "$LOG" 2>/dev/null || true
        echo "  以低权限用户 $RUN_AS 运行"
        nohup runuser -u "$RUN_AS" -- "$BIN" -config "$CONFIG" $INSECURE >>"$LOG" 2>&1 &
    else
        if [ -n "${RUN_AS:-}" ] && [ "$RUN_AS" != "root" ]; then
            echo "⚠ 无法以 $RUN_AS 运行（缺 runuser 或用户不存在），改为 root" >&2
        fi
        nohup "$BIN" -config "$CONFIG" $INSECURE >>"$LOG" 2>&1 &
    fi
}

# 临时目录放在 root 专属目录：/tmp 常被 noexec 挂载，且可预测的
# 文件名会给本地用户留下抢占路径（BIN_DIR 自更新场景可能归 Agent 用户）。
WORK=$(mktemp -d /root/.rollcall-install.XXXXXX 2>/dev/null \
    || mktemp -d "${TMPDIR:-/tmp}/rollcall-install.XXXXXX") || {
    echo "无法创建临时目录" >&2; exit 1
}
TMP="$WORK/agent"
trap 'rm -rf "$WORK"' EXIT HUP INT TERM
echo "→ 下载 Agent ($ARCH)"
fetch_file "$SERVER/agent/download?arch=$ARCH" "$TMP" || {
    echo "下载失败: $SERVER/agent/download?arch=$ARCH" >&2
    exit 1
}
# 校验和由同一控制端提供：能发现传输截断/代理缓存错误，
# 也能避免把错误页当成二进制装上去（身份校验仍然保留在下面）。
EXPECTED=$(fetch_text "$SERVER/agent/download.sha256?arch=$ARCH") || {
    echo "无法获取二进制校验值，已放弃安装" >&2
    exit 1
}
EXPECTED=$(printf '%%s' "$EXPECTED" | tr -d ' \r\n' | tr 'A-F' 'a-f')
case "$EXPECTED" in
    *[!0-9a-f]*|'')
        echo "校验值格式无效，已放弃安装" >&2
        exit 1
        ;;
esac
[ "${#EXPECTED}" -eq 64 ] || {
    echo "校验值长度无效，已放弃安装" >&2
    exit 1
}
if command -v sha256sum >/dev/null 2>&1; then
    ACTUAL=$(sha256sum "$TMP" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
    ACTUAL=$(shasum -a 256 "$TMP" | awk '{print $1}')
elif command -v openssl >/dev/null 2>&1; then
    ACTUAL=$(openssl dgst -sha256 "$TMP" | awk '{print $NF}')
else
    echo "缺少 sha256sum / shasum / openssl，无法校验下载的二进制" >&2
    exit 1
fi
if [ "$ACTUAL" != "$EXPECTED" ]; then
    echo "SHA-256 不匹配（期望 $EXPECTED，实际 $ACTUAL），已放弃安装" >&2
    exit 1
fi
chmod 0755 "$TMP"

# 先在临时路径上验证能执行，并确认确实是 Agent（防止拿到错误文件/代理错误页）
if ! NEW_VER=$("$TMP" -version 2>/dev/null); then
    echo "下载的二进制无法执行，已放弃安装" >&2
    rm -f "$TMP"
    exit 1
fi
case "$NEW_VER" in
    rollcall-agent\ *) ;;
    *)
        echo "下载的文件不是 Rollcall Agent（-version 输出: ${NEW_VER:-空}），已放弃安装" >&2
        rm -f "$TMP"
        exit 1
        ;;
esac

if [ "$UPGRADE" = "1" ]; then
    OLD_VER=$("$BIN" -version 2>/dev/null || true)
    case "$OLD_VER" in
        rollcall-agent\ *) ;;
        *)
            # 运行单元可能用了 env/wrapper，从 ExecStart 取到的路径未必是 Agent 本体。
            echo "$BIN 不是 Rollcall Agent（-version 输出: ${OLD_VER:-空}）" >&2
            echo "请用 --upgrade <Agent 二进制路径> 指定" >&2
            rm -f "$TMP"
            exit 1
            ;;
    esac
    # 记录当前运行身份：无 systemd 的回退模式重启时要沿用，
    # 不能因为升级把低权限进程变成 root 常驻。
    RUN_AS=$(agent_run_user)
    [ -n "$RUN_AS" ] || RUN_AS=$(agent_user_from_config)
    # 保留原属主：自更新目录通常归 rollcall-agent，换成 root 会让下次自更新失效。
    OWNER=$(stat -c '%%u:%%g' "$BIN" 2>/dev/null || true)
    # 先写随机目录再 rename：避免 BIN.prev 被预置成指向敏感文件的软链接。
    BACKUP="$WORK/agent.prev"
    cp -p "$BIN" "$BACKUP"
    mv -f "$BACKUP" "$BIN.prev"
    mv -f "$TMP" "$BIN"
    if [ -n "$OWNER" ]; then
        chown "$OWNER" "$BIN" 2>/dev/null || true
    fi
    echo "✔ Agent 已替换: $OLD_VER → $NEW_VER"
    echo "  配置未改动；旧版本备份在 $BIN.prev"

    if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1 && systemctl cat rollcall-agent.service >/dev/null 2>&1; then
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
        # 无 systemd 的容器/LXC：升级同样要重启，否则新二进制永远不会生效。
        echo "→ 未检测到 rollcall-agent.service，重启后台进程"
        stop_agent
        start_agent_bg
        sleep 2
        if [ -n "$(agent_pids)" ]; then
            echo "✔ Agent 已以新版本重启，约 30 秒内可在面板看到"
        else
            echo "✘ 新版本启动失败，正在回滚…" >&2
            mv -f "$BIN.prev" "$BIN"
            start_agent_bg
            sleep 2
            if [ -n "$(agent_pids)" ]; then
                echo "✔ 已回滚到 $OLD_VER" >&2
            else
                echo "✘ 回滚后仍未启动，查看 /var/log/rollcall-agent.log" >&2
            fi
            exit 1
        fi
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
if [ "$SANDBOX" = "1" ]; then
    # 沙箱不创建系统用户：演练环境以 root 跑在隔离前缀里，随前缀一起删除。
    install -d -m 0700 "$ETC_DIR"
elif [ -n "$AGENT_USER" ]; then
    if id -u "$AGENT_USER" >/dev/null 2>&1; then
        AS_USER=$AGENT_USER
    elif command -v useradd >/dev/null 2>&1; then
        # useradd 在某些容器里存在但被限制；失败就退回 root 运行而不是中断安装。
        useradd --system --no-create-home --shell /usr/sbin/nologin "$AGENT_USER" 2>/dev/null || true
        id -u "$AGENT_USER" >/dev/null 2>&1 && AS_USER=$AGENT_USER
    fi
fi

if [ -n "$AS_USER" ]; then
    # 组名要查主组，不能假定与用户名相同：部分发行版/容器镜像里
    # USERGROUPS_ENAB=no，useradd 不会创建同名组，Group= 写错会让服务起不来。
    AS_GROUP=$(id -gn "$AS_USER" 2>/dev/null || echo "$AS_USER")
    SVC_USER="User=$AS_USER
Group=$AS_GROUP"
    install -d -m 0750 -o root -g "$AS_GROUP" "$ETC_DIR"
else
    if [ "$SANDBOX" != "1" ]; then
        echo "⚠ 未能创建低权限用户 $AGENT_USER（可能缺少 useradd），Agent 将以 root 运行" >&2
    fi
    SVC_USER=""
    [ "$SANDBOX" = "1" ] || install -d -m 0700 "$ETC_DIR"
fi

umask 077
cat > "$CONFIG" <<JSON
{
  "server": "$SERVER_JSON",
  "nodeId": "$NODE_ID_JSON",
  "token": "$TOKEN_JSON",
  "interval": 30,
  "update": { "enabled": false }
}
JSON
if [ -n "$AS_USER" ]; then
    chown root:"$AS_USER" "$CONFIG"
    chmod 0640 "$CONFIG"
else
    chmod 0600 "$CONFIG"
fi

cat > "$UNIT_FILE" <<UNIT
[Unit]
Description=Rollcall Agent (read-only metrics reporter)
Documentation=https://github.com/motao123/rollcall
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
$SVC_USER
ExecStart=$BIN -config $CONFIG$INSECURE
Restart=always
RestartSec=10s
ReadOnlyPaths=/proc

# 采集器只读：无特权、只读文件系统、禁写可执行内存。
# 与 deploy/rollcall-agent.service 保持逐项一致（含 PrivateDevices/
# RestrictNamespaces），否则两种安装方式的安全姿态会悄悄分叉。
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_INET AF_INET6
RestrictNamespaces=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true

[Install]
WantedBy=multi-user.target
UNIT

# 容器/LXC 里常没有可用的 systemd：退回直接后台运行，
# 否则整条安装会因为 systemctl 不存在而失败。
# 仅看 systemctl 是否存在不够：容器里常有客户端但没有 PID 1 的 systemd。
# 沙箱模式同样走这里：不碰系统 systemd，进程随 RC_PREFIX 一起清理。
if [ "$SANDBOX" = "1" ] || [ ! -d /run/systemd/system ] || ! command -v systemctl >/dev/null 2>&1; then
    if [ "$SANDBOX" != "1" ]; then
        echo "→ 未检测到运行中的 systemd，改为后台常驻"
    fi
    RUN_AS=$AS_USER
    # 与 systemd 分支保持同样的最小权限：有专用用户就不要 root 常驻。
    stop_agent
    start_agent_bg
    sleep 2
    if [ -n "$(agent_pids)" ]; then
        if [ "$SANDBOX" = "1" ]; then
            echo "✔ 沙箱 Agent 已在后台运行（root，日志 $LOG_FILE），约 30 秒内可在面板看到该节点"
            echo "  systemd 单元样例（未启用）: $UNIT_FILE"
            echo "  清理: pkill -f '$BIN' ; rm -rf '$RC_PREFIX'"
        else
            echo "✔ Agent 已在后台运行，约 30 秒内可在面板看到该节点"
            echo "  日志: /var/log/rollcall-agent.log（无 systemd，开机自启需自行配置）"
        fi
    else
        echo "✘ Agent 启动失败，查看 $LOG_FILE" >&2
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
	if !validAgentArch(arch) {
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

func validAgentArch(arch string) bool {
	switch arch {
	case "amd64", "arm64", "arm", "386":
		return true
	default:
		return false
	}
}

// handleAgentChecksum 返回分发目录中 Agent 二进制的 SHA-256（小写十六进制）。
// 安装脚本下载后校验，避免截断文件或代理错误页被当作 Agent 安装。
func (s *Server) handleAgentChecksum(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	arch := r.URL.Query().Get("arch")
	if !validAgentArch(arch) {
		fail(w, http.StatusBadRequest, "arch 必须是 amd64/arm64/arm/386")
		return
	}
	if s.agentDir == "" {
		fail(w, http.StatusNotFound, "服务端未启用 Agent 分发（启动参数 -agent-dir 未配置）")
		return
	}
	name := "rollcall-agent-linux-" + arch
	full := filepath.Join(s.agentDir, name)
	// 与下载接口一致：必须是常规文件。目录/设备/FIFO 会让 io.Copy 阻塞
	// （FIFO 尤其：请求会一直挂住，占住连接直到超时）。
	fi, serr := os.Stat(full)
	if serr != nil || !fi.Mode().IsRegular() {
		fail(w, http.StatusNotFound, "该架构的 Agent 尚未放到分发目录: "+name)
		return
	}
	f, err := os.Open(full)
	if err != nil {
		fail(w, http.StatusNotFound, "该架构的 Agent 尚未放到分发目录: "+name)
		return
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		fail(w, http.StatusInternalServerError, "读取 Agent 失败")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = io.WriteString(w, hex.EncodeToString(h.Sum(nil))+"\n")
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

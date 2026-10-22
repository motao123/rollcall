#!/bin/sh
# Rollcall 服务端一键升级 / 回滚。
#
# 升级：校验新二进制的 SHA-256；配置了发行公钥时必须先通过 Ed25519
#       发行签名校验（签名绑定版本号、SHA-256 与大小），再备份当前版本为
#       <bin>.prev，原子替换后重启 systemd 服务并做健康检查；失败自动回滚。
# 回滚：把本机此前安装过的 <bin>.prev 恢复为当前版本并重启；
#       恢复的是本机文件，不需要重新验签。
#
# 用法：
#   rollcall-server-upgrade.sh upgrade --file /tmp/rollcall-server --sha256 <64位hex> \
#     --signature /tmp/rollcall-server.sig
#   rollcall-server-upgrade.sh upgrade --url https://example.com/rollcall-server --sha256 <64位hex> \
#     --signature /tmp/rollcall-server.sig
#   rollcall-server-upgrade.sh rollback
#
# 发行签名：默认公钥为 /etc/rollcall/release.pub；该文件存在时必须验签。
#           --signature <文件>          新二进制的 .sig 签名文件
#           --pubkey <路径>             发行公钥；默认 /etc/rollcall/release.pub
#           --rollsign <路径>           验签工具；默认 /opt/rollcall/rollsign，
#                                       其次 PATH 中的 rollsign
#           --allow-unsigned            未配置公钥时显式跳过验签（不推荐）
#
# 可选参数：--service rollcall-server   目标 systemd 单元
#           --bin <路径>                要替换的二进制；默认取该单元 ExecStart
#                                       里的路径，取不到时才回退
#                                       /opt/rollcall/rollcall-server
#           --health http://127.0.0.1:19080/readyz
#           --health "" 跳过健康检查
#
# 只支持 Linux systemd 部署；需要 root 权限。脚本不读取任何凭据。

set -eu

BIN=/opt/rollcall/rollcall-server
BIN_EXPLICIT=0
SERVICE=rollcall-server
HEALTH=http://127.0.0.1:19080/readyz
PUBKEY=/etc/rollcall/release.pub
PUBKEY_EXPLICIT=0
SIGNATURE=
ROLLSIGN=
ALLOW_UNSIGNED=0
LOCK=/run/rollcall-upgrade.lock
STAGE=
PREV=
# 非空时要求 /readyz 报告的版本与目标二进制一致，
# 防止 systemd 仍拉起旧文件/旧路径时误判升级成功。
EXPECT_VERSION=

log() { printf '%s\n' "[rollcall-upgrade] $*"; }
die() { printf '%s\n' "[rollcall-upgrade] 错误: $*" >&2; exit 1; }

# 只取开头的连续注释块：写死行号（sed -n '2,20p'）会在注释增减后
# 把 set -eu 之类的正文一起打进帮助信息。
usage() {
  sed -n '2,${/^#/!q; s/^# \{0,1\}//; p;}' "$0"
}

cleanup() {
  [ -n "$STAGE" ] && rm -f "$STAGE"
  [ -n "$PREV" ] && rm -f "$PREV"
  # 锁目录里现在有 pid 文件，rmdir 会失败（目录非空），必须整目录删。
  # $LOCK 是本脚本创建的固定路径，不会误删别的东西。
  rm -rf "$LOCK"
}
trap cleanup EXIT INT TERM

need_root() {
  [ "$(id -u)" = 0 ] || die "请使用 root 运行（sudo sh $0 ...）"
}

need_systemd() {
  command -v systemctl >/dev/null 2>&1 || die "未找到 systemctl，本脚本只支持 systemd"
}

# unit_bin 取目标服务 ExecStart 里的真实二进制路径。
unit_bin() {
  systemctl show -p ExecStart --value "$SERVICE" 2>/dev/null |
    sed -n 's/.*path=\([^ ;]*\).*/\1/p' | head -n 1
}

# need_unit 在动任何文件之前确认单元存在。
# systemctl show 对不存在的单元同样退出 0 并返回空属性，
# 只靠 ExecStart 是否为空无法区分「单元不存在」和「ExecStart 写法特殊」；
# 前者必须直接失败，否则脚本会去替换默认路径的二进制、再 restart 一个不存在的单元。
need_unit() {
  state=$(systemctl show -p LoadState --value "$SERVICE" 2>/dev/null || true)
  [ "$state" != "not-found" ] || die "systemd 单元不存在: $SERVICE"
}

# resolve_bin 让 --service 决定要替换哪个文件。
# BIN 与 SERVICE 曾是两个互不校验的参数：只传 --service 时脚本仍按默认的
# /opt/rollcall/rollcall-server 替换文件，却去重启另一个服务。健康检查只有
# 在版本号对不上时才会回滚，一旦 --health 留空或两者版本恰好相同，
# 就会静默改掉一个不会被重启的实例的二进制。
resolve_bin() {
  need_unit
  ub=$(unit_bin)
  if [ "$BIN_EXPLICIT" = 1 ]; then
    if [ -n "$ub" ] && [ "$ub" != "$BIN" ]; then
      die "--bin ($BIN) 与服务 $SERVICE 的 ExecStart ($ub) 不一致：会替换掉一个不会被重启的实例。请改用 --bin $ub，或指定正确的 --service"
    fi
    return 0
  fi
  if [ -n "$ub" ]; then
    BIN=$ub
  else
    log "⚠ 无法从 $SERVICE 的 ExecStart 推导二进制路径，使用默认 $BIN"
  fi
}

# 健康检查是升级唯一的有效判据。缺 HTTP 客户端时必须直接失败，
# 不能在 health_ok 里 return 0 把「没检查」当成「检查通过」。
need_http_client() {
  [ -n "$HEALTH" ] || return 0
  command -v curl >/dev/null 2>&1 && return 0
  command -v wget >/dev/null 2>&1 && return 0
  die "健康检查需要 curl 或 wget；确实要跳过请用 --health \"\""
}

# need_rollsign 定位本机的验签工具。验签只信任本机已安装的 rollsign
# 与公钥，不从下载源获取；找不到时必须在替换任何文件之前失败。
need_rollsign() {
  if [ -n "$ROLLSIGN" ]; then
    [ -x "$ROLLSIGN" ] || die "验签工具不可执行: $ROLLSIGN"
    return 0
  fi
  if [ -x /opt/rollcall/rollsign ]; then
    ROLLSIGN=/opt/rollcall/rollsign
    return 0
  fi
  if command -v rollsign >/dev/null 2>&1; then
    ROLLSIGN=$(command -v rollsign)
    return 0
  fi
  die "已启用发行签名校验，但找不到 rollsign：请安装到 /opt/rollcall/rollsign，或用 --rollsign 指定"
}

# verify_release 校验签名并输出签名中的版本号。
# 必须在新二进制落位之前调用；失败时当前运行版本不受影响。
verify_release() {
  need_rollsign
  [ -n "$PUBKEY" ] || die "发行公钥路径为空"
  [ -f "$PUBKEY" ] || die "找不到发行公钥: $PUBKEY"
  [ -n "$SIGNATURE" ] || die "已配置发行公钥（$PUBKEY），必须提供 --signature"
  [ -f "$SIGNATURE" ] || die "找不到发行签名文件: $SIGNATURE"
  signed=$("$ROLLSIGN" verify-release -pubkey "$PUBKEY" -file "$1" -signature "$SIGNATURE") ||
    die "发行签名校验失败（未改动当前版本）"
  [ -n "$signed" ] || die "验签工具未返回版本号"
  printf '%s' "$signed"
}

# proc_starttime 取进程启动时间（/proc/<pid>/stat 第 22 字段）。
# 只判 kill -0 不够：升级进程被 kill -9 后 PID 可能被系统复用给别的进程，
# 那样"锁还有人持有"会永远成立，升级再也做不了。
# comm 里可能有空格与括号，先把 "pid (comm) " 整段剥掉，再取第 20 列
# （原始第 3 列 state 变成第 1 列，故 22-2=20）。
proc_starttime() {
  sed 's/^[0-9]* (.*) //' "/proc/$1/stat" 2>/dev/null | awk '{print $20}'
}

# lock_holder_alive 判断锁是否仍被活着的进程持有。
lock_holder_alive() {
  [ -f "$LOCK/pid" ] || return 1
  pid=
  started=
  read -r pid started <"$LOCK/pid" 2>/dev/null || true
  [ -n "$pid" ] || return 1
  now_started=$(proc_starttime "$pid")
  # 进程不存在，或 PID 已被复用成别的进程（启动时间对不上）→ 锁已失效
  [ -n "$now_started" ] && [ "$now_started" = "$started" ]
}

# take_lock 用目录做互斥，并回收「上一个进程被强杀后遗留」的锁。
# 只 mkdir 不回收的话，一次 kill -9 或断电就能把升级永久锁死，
# 而报错文案只会说"另一个升级正在进行"，运维很难判断是假锁。
take_lock() {
  if mkdir "$LOCK" 2>/dev/null; then
    printf '%s %s\n' "$$" "$(proc_starttime $$)" >"$LOCK/pid"
    return 0
  fi
  if lock_holder_alive; then
    die "另一个升级/回滚正在进行（$LOCK，pid=$(cut -d' ' -f1 "$LOCK/pid" 2>/dev/null)）"
  fi
  # 没有 pid 文件 = 上个进程刚创建锁就被杀；按陈旧时间兜底，
  # 避免把"正在写 pid"的窗口误判成遗留锁。
  if [ ! -f "$LOCK/pid" ]; then
    if [ -z "$(find "$LOCK" -maxdepth 0 -mmin +10 2>/dev/null)" ]; then
      die "另一个升级/回滚正在进行（$LOCK 刚被创建，尚无 pid 文件）"
    fi
    log "⚠ 回收遗留锁（无 pid 文件且已超过 10 分钟）: $LOCK"
  else
    log "⚠ 回收遗留锁（原进程已退出）: $LOCK"
  fi
  rm -rf "$LOCK"
  mkdir "$LOCK" 2>/dev/null || die "无法获取升级锁（$LOCK）"
  printf '%s %s\n' "$$" "$(proc_starttime $$)" >"$LOCK/pid"
}

hash_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  elif command -v openssl >/dev/null 2>&1; then
    openssl dgst -sha256 "$1" | awk '{print $NF}'
  else
    die "缺少 sha256sum / shasum / openssl，无法校验二进制"
  fi
}

download() {
  src=$1
  dst=$2
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --retry 3 --connect-timeout 10 -o "$dst" "$src" ||
      die "下载失败: $src"
  elif command -v wget >/dev/null 2>&1; then
    wget -q -T 30 -O "$dst" "$src" || die "下载失败: $src"
  else
    die "缺少 curl 或 wget，无法下载二进制"
  fi
}

health_ok() {
  [ -n "$HEALTH" ] || return 0
  i=0
  while [ "$i" -lt 20 ]; do
    if systemctl is-active --quiet "$SERVICE" 2>/dev/null; then
      body=
      fetched=0
      if command -v curl >/dev/null 2>&1; then
        if body=$(curl -fsS --max-time 3 "$HEALTH" 2>/dev/null); then fetched=1; fi
      elif command -v wget >/dev/null 2>&1; then
        if body=$(wget -q -T 3 -O - "$HEALTH" 2>/dev/null); then fetched=1; fi
      else
        return 1
      fi
      if [ "$fetched" = 1 ]; then
        # 严格取值再比对，而不是 grep 子串：`"version":"v1.14.1"` 是子串匹配，
        # 拼错/多字段时可能命中别的位置；这里的语义是「版本必须完全相等」。
        # 不依赖 jq（目标机可能没有），用 sed 取第一个 version 字段。
        reported=$(printf '%s' "$body" |
          sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)
        if [ -z "$EXPECT_VERSION" ] || [ "$reported" = "$EXPECT_VERSION" ]; then
          return 0
        fi
      fi
    fi
    sleep 1
    i=$((i + 1))
  done
  return 1
}

# binary_version 从 `-version` 输出中取第二列（rollcall-server v1.2.3 -> v1.2.3）。
binary_version() {
  "$1" -version 2>/dev/null | awk 'NR==1 {print $2}'
}

restart_and_check() {
  log "重启服务 $SERVICE"
  systemctl restart "$SERVICE"
  if health_ok; then
    return 0
  fi
  log "健康检查未通过（$HEALTH）"
  systemctl status "$SERVICE" --no-pager -n 20 >&2 || true
  return 1
}

# restore_prev 用 .prev 原子替换当前二进制（保留 .prev 作为再次回滚的备份）。
restore_prev() {
  [ -f "$BIN.prev" ] || die "没有可回滚的备份: $BIN.prev"
  # 同目录随机名再 rename：可预测的 $BIN.restore.$$ 能被本地用户预置成
  # 软链接，把备份写到别处（与 install.sh 的做法保持一致）。
  PREV=$(mktemp "$(dirname "$BIN")/.rollcall-restore.XXXXXX") || die "创建临时文件失败"
  cp -p "$BIN.prev" "$PREV" || die "复制备份失败"
  chmod 0755 "$PREV" 2>/dev/null || true
  sync
  mv -f "$PREV" "$BIN" || die "替换二进制失败"
  PREV=
}

cmd_upgrade() {
  file=$1
  url=$2
  want=$3

  [ -n "$want" ] || die "必须提供 --sha256（新二进制的 SHA-256），不允许未校验升级"
  [ -n "$file" ] || [ -n "$url" ] || die "请提供 --file 或 --url"
  [ -z "$file" ] || [ -z "$url" ] || die "--file 与 --url 只能二选一"
  need_systemd
  resolve_bin
  need_http_client
  [ -f "$BIN" ] || die "未找到当前版本: $BIN（首次安装请使用部署文档中的安装步骤）"
  take_lock

  # ★ 暂存文件必须落在 $BIN 所在目录：`mv` 只有同文件系统内才是原子的。
  # 放 /tmp 时若 /tmp 是 tmpfs（生产机就是），mv 退化为「拷贝 + 删除」，
  # 中途断电/崩溃会在服务路径上留下截断的二进制，服务直接起不来。
  # 目录不可写时（只读挂载、权限不足）在这里就失败，不会等到替换阶段。
  stage_dir=$(dirname "$BIN")
  STAGE=$(mktemp "$stage_dir/.rollcall-upgrade.XXXXXX") ||
    die "在 $stage_dir 创建暂存文件失败（目录是否可写？）"
  new=$STAGE
  if [ -n "$url" ]; then
    log "下载 $url"
    download "$url" "$new"
  else
    [ -f "$file" ] || die "文件不存在: $file"
    # 同文件系统内 cp 也更快，且不会因为源在 /tmp 而把暂存文件放错位置。
    cp "$file" "$new" || die "复制新二进制失败"
  fi

  got=$(hash_file "$new" | tr 'A-F' 'a-f')
  want=$(printf '%s' "$want" | tr 'A-F' 'a-f')
  [ "$got" = "$want" ] || die "SHA-256 不匹配：期望 $want，实际 $got"

  magic=$(od -An -tx1 -N4 "$new" | tr -d ' \n')
  [ "$magic" = "7f454c46" ] || die "新文件不是 Linux ELF 可执行文件"

  # 验签不可被静默跳过：一旦配置了公钥或显式提供签名，
  # --allow-unsigned 就是参数冲突，而不是"覆盖验签"。
  if [ "$ALLOW_UNSIGNED" = 1 ] &&
    { [ -n "$SIGNATURE" ] || [ "$PUBKEY_EXPLICIT" = 1 ] || [ -f "$PUBKEY" ]; }; then
    die "--allow-unsigned 不能与发行公钥或 --signature 同时使用：验签不可被跳过"
  fi

  chmod 0755 "$new" || die "设置新二进制执行权限失败"
  if [ -n "$SIGNATURE" ] || [ "$PUBKEY_EXPLICIT" = 1 ] || [ -f "$PUBKEY" ]; then
    # 先验签再执行新二进制：签名版本还必须与二进制自报版本一致，
    # 防止签的是别的构建产物（例如错误的架构或旧文件）却通过了哈希。
    EXPECT_VERSION=$(verify_release "$new")
    reported=$(binary_version "$new" || true)
    [ "$reported" = "$EXPECT_VERSION" ] ||
      die "签名版本 $EXPECT_VERSION 与二进制报告的版本 ${reported:-未知} 不一致，已中止（未改动当前版本）"
    log "发行签名校验通过（版本 $EXPECT_VERSION）"
  elif [ "$ALLOW_UNSIGNED" = 1 ]; then
    log "⚠ 未启用发行签名校验（--allow-unsigned）：只校验 SHA-256，无法抵御下载源被篡改"
    EXPECT_VERSION=$(binary_version "$new" || true)
    [ -n "$EXPECT_VERSION" ] || log "提示: 无法读取新版本号，只校验就绪状态"
  else
    die "未配置发行公钥（$PUBKEY）：请安装公钥并提供 --signature，或在明确接受风险后用 --allow-unsigned 跳过验签"
  fi

  old_ver=$("$BIN" -version 2>/dev/null || printf 'unknown')
  log "当前版本: $old_ver"
  log "备份当前二进制到 $BIN.prev"
  cp -p "$BIN" "$BIN.prev" || die "备份当前版本失败"
  sync

  log "安装新二进制"
  sync
  mv -f "$new" "$BIN" || die "替换二进制失败"

  if ! restart_and_check; then
    log "新版本启动失败，自动回滚"
    restore_prev
    # 必须按恢复后的二进制重新取版本：沿用新版本号会让 /readyz
    # 的版本核对永远失败，把一次成功的回滚误报成「回滚后仍不健康」。
    EXPECT_VERSION=$(binary_version "$BIN" || true)
    if restart_and_check; then
      log "已回滚到上一版本，服务恢复正常"
    else
      log "回滚后健康检查仍未通过，请人工检查 systemctl status $SERVICE"
    fi
    exit 1
  fi

  new_ver=$("$BIN" -version 2>/dev/null || printf 'unknown')
  log "升级完成: $old_ver -> $new_ver"
  log "回滚命令: sh $0 rollback"
}

cmd_rollback() {
  need_systemd
  resolve_bin
  need_http_client
  [ -f "$BIN.prev" ] || die "没有可回滚的备份: $BIN.prev"
  take_lock

  failed_ver=$("$BIN" -version 2>/dev/null || printf 'unknown')
  EXPECT_VERSION=$(binary_version "$BIN.prev" || true)
  log "当前版本: $failed_ver"
  log "从 $BIN.prev 恢复"
  restore_prev

  if ! restart_and_check; then
    log "回滚后健康检查未通过，请人工检查 systemctl status $SERVICE"
    log "备份仍保留在 $BIN.prev"
    exit 1
  fi

  ver=$("$BIN" -version 2>/dev/null || printf 'unknown')
  log "回滚完成: $failed_ver -> $ver"
}

cmd=
file=
url=
sha=
while [ $# -gt 0 ]; do
  case "$1" in
    upgrade | rollback) cmd=$1 ;;
    --file) shift; file=${1:-} ;;
    --url) shift; url=${1:-} ;;
    --sha256) shift; sha=${1:-} ;;
    --bin) shift; BIN=${1:-$BIN}; BIN_EXPLICIT=1 ;;
    --service) shift; SERVICE=${1:-$SERVICE} ;;
    --health) shift; HEALTH=${1:-} ;;
    --signature) shift; SIGNATURE=${1:-} ;;
    --pubkey) shift; PUBKEY=${1:-}; PUBKEY_EXPLICIT=1 ;;
    --rollsign) shift; ROLLSIGN=${1:-} ;;
    --allow-unsigned) ALLOW_UNSIGNED=1 ;;
    -h | --help | help) usage; exit 0 ;;
    *) die "未知参数: $1（--help 查看用法）" ;;
  esac
  shift
done

case "$cmd" in
  upgrade) need_root; cmd_upgrade "$file" "$url" "$sha" ;;
  rollback) need_root; cmd_rollback ;;
  *) usage; exit 2 ;;
esac

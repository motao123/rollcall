#!/bin/sh
# Rollcall 服务端一键升级 / 回滚。
#
# 升级：先校验新二进制的 SHA-256，备份当前版本为 <bin>.prev，
#       原子替换后重启 systemd 服务并做健康检查；失败时自动回滚。
# 回滚：把 <bin>.prev 恢复为当前版本并重启，同样做健康检查。
#
# 用法：
#   rollcall-server-upgrade.sh upgrade --file /tmp/rollcall-server --sha256 <64位hex>
#   rollcall-server-upgrade.sh upgrade --url https://example.com/rollcall-server --sha256 <64位hex>
#   rollcall-server-upgrade.sh rollback
#
# 可选参数：--bin /opt/rollcall/rollcall-server
#           --service rollcall-server
#           --health http://127.0.0.1:19080/healthz
#           --health "" 跳过健康检查
#
# 只支持 Linux systemd 部署；需要 root 权限。脚本不读取任何凭据。

set -eu

BIN=/opt/rollcall/rollcall-server
SERVICE=rollcall-server
HEALTH=http://127.0.0.1:19080/healthz
LOCK=/run/rollcall-upgrade.lock
TMP=
PREV=

log() { printf '%s\n' "[rollcall-upgrade] $*"; }
die() { printf '%s\n' "[rollcall-upgrade] 错误: $*" >&2; exit 1; }

usage() {
  sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'
}

cleanup() {
  [ -n "$TMP" ] && rm -rf "$TMP"
  [ -n "$PREV" ] && rm -f "$PREV"
  rmdir "$LOCK" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

need_root() {
  [ "$(id -u)" = 0 ] || die "请使用 root 运行（sudo sh $0 ...）"
}

need_systemd() {
  command -v systemctl >/dev/null 2>&1 || die "未找到 systemctl，本脚本只支持 systemd"
}

take_lock() {
  mkdir "$LOCK" 2>/dev/null || die "另一个升级/回滚正在进行（$LOCK）"
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
      if command -v curl >/dev/null 2>&1; then
        curl -fsS --max-time 3 "$HEALTH" >/dev/null 2>&1 && return 0
      elif command -v wget >/dev/null 2>&1; then
        wget -q -T 3 -O /dev/null "$HEALTH" && return 0
      else
        return 0
      fi
    fi
    sleep 1
    i=$((i + 1))
  done
  return 1
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
  PREV="$BIN.restore.$$"
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
  [ -f "$BIN" ] || die "未找到当前版本: $BIN（首次安装请使用部署文档中的安装步骤）"
  need_systemd
  take_lock

  TMP=$(mktemp -d "${TMPDIR:-/tmp}/rollcall-upgrade.XXXXXX") || die "创建临时目录失败"
  new=$TMP/rollcall-server
  if [ -n "$url" ]; then
    log "下载 $url"
    download "$url" "$new"
  else
    [ -f "$file" ] || die "文件不存在: $file"
    cp "$file" "$new" || die "复制新二进制失败"
  fi

  got=$(hash_file "$new" | tr 'A-F' 'a-f')
  want=$(printf '%s' "$want" | tr 'A-F' 'a-f')
  [ "$got" = "$want" ] || die "SHA-256 不匹配：期望 $want，实际 $got"

  magic=$(od -An -tx1 -N4 "$new" | tr -d ' \n')
  [ "$magic" = "7f454c46" ] || die "新文件不是 Linux ELF 可执行文件"

  old_ver=$("$BIN" -version 2>/dev/null || printf 'unknown')
  log "当前版本: $old_ver"
  log "备份当前二进制到 $BIN.prev"
  cp -p "$BIN" "$BIN.prev" || die "备份当前版本失败"
  sync

  log "安装新二进制"
  chmod 0755 "$new"
  sync
  mv -f "$new" "$BIN" || die "替换二进制失败"

  if ! restart_and_check; then
    log "新版本启动失败，自动回滚"
    restore_prev
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
  [ -f "$BIN.prev" ] || die "没有可回滚的备份: $BIN.prev"
  need_systemd
  take_lock

  failed_ver=$("$BIN" -version 2>/dev/null || printf 'unknown')
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
    --bin) shift; BIN=${1:-$BIN} ;;
    --service) shift; SERVICE=${1:-$SERVICE} ;;
    --health) shift; HEALTH=${1:-} ;;
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

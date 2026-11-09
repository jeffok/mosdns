#!/bin/sh
# apply.sh — 把 staging 规则应用为线上规则：校验 → 金丝雀 → 原子切换 → 重启 → 后检 → 失败回滚
#
# 关键约束（方案 §19.6 / GS-3~GS-5）：
#   * 任何一步失败，主实例【不能】重启（校验失败、金丝雀失败）；
#   * 重启后哨兵查询不通过 → 自动回滚到上一版并再次后检；
#   * 全程用 flock 互斥，避免同步循环与手工 --now 并发；
#   * 内容哈希没变就直接退出（GS-1：无变化不重启）。
#
# 用法：apply.sh [--now] [--force]
#   --now    立即执行（否则只做一次「是否有变化」的判断）
#   --force  忽略哈希比较，强制走完整流程（测试用）
set -u

STATE_DIR="${STATE_DIR:-/data/state}"
RULES="${RULES_DIR:-/etc/mosdns/rules}"
CONFIG="${MOSDNS_CONFIG:-/etc/mosdns/config.yaml}"
STAGING="${STAGING:-$STATE_DIR/staging}"
LKG_DIR="${LKG_DIR:-$STATE_DIR/lkg}"
PREV_DIR="${PREV_DIR:-$STATE_DIR/prev}"
HISTORY_DIR="${HISTORY_DIR:-$STATE_DIR/history}"
HISTORY_KEEP="${HISTORY_KEEP:-20}"
LOCK_FILE="${LOCK_FILE:-$STATE_DIR/apply.lock}"
DIG_PORT="${DNS_ENTRY_PORT:-53}"
CANARY_PORT="${CANARY_PORT:-15353}"
CANARY_API="${CANARY_API:-19091}"
CANARY_SENTINEL="${CANARY_SENTINEL:-www.baidu.com}"
RESTART_TIMEOUT="${RESTART_TIMEOUT:-20}"
PID_FILE="${PID_FILE:-/tmp/mosdns.pid}"
FORCE=0
for a in "$@"; do
  case "$a" in
    --now) ;;   # 兼容参数：立即执行就是默认行为
    --force) FORCE=1 ;;
    -h|--help) sed -n '2,16p' "$0"; exit 0 ;;
  esac
done

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)

log() { echo "[$(date '+%H:%M:%S')] [apply] $*"; }
warn() { echo "[$(date '+%H:%M:%S')] [apply] WARN $*" >&2; }
err() { echo "[$(date '+%H:%M:%S')] [apply] ERROR $*" >&2; }
now_ms() {
  awk '{ printf "%.0f", $1 * 1000 }' /proc/uptime 2>/dev/null \
    || echo $(( $(date +%s) * 1000 ))
}

mkdir -p "$STATE_DIR" "$RULES"

# ---------- 互斥 ----------
if command -v flock >/dev/null 2>&1; then
  exec 9>"$LOCK_FILE"
  if ! flock -n 9; then
    warn "另一个 apply 正在运行，本次跳过"
    exit 0
  fi
fi

# ---------- 变化检测（GS-1） ----------
# 哈希只看「规则内容 + custom-hosts」，文件时间戳不参与
if [ ! -f "$STAGING/custom-hosts.txt" ] && [ -f "$RULES/custom-hosts.txt" ]; then
  mkdir -p "$STAGING"
  cp "$RULES/custom-hosts.txt" "$STAGING/custom-hosts.txt"
fi
# Complete the candidate with local support files before comparing content.
for support in custom-local.txt accept-extra.txt ai-list.txt; do
  if [ ! -f "$STAGING/$support" ] && [ -f "$RULES/$support" ]; then
    mkdir -p "$STAGING"
    cp "$RULES/$support" "$STAGING/$support" || exit 1
  fi
done
normalize_staging() {
  src="$STAGING/bogus-nxdomain.china.conf"
  [ -f "$src" ] || return 0
  tmp="$STAGING/bogus-ip.txt.new"
  {
    echo "# generated from bogus-nxdomain.china.conf"
    grep -oE '^bogus-nxdomain=[0-9]+[.][0-9]+[.][0-9]+[.][0-9]+$' "$src" \
      | sed 's/^bogus-nxdomain=//' | sort -u
  } > "$tmp" || return 1
  mv "$tmp" "$STAGING/bogus-ip.txt"
}

if ! normalize_staging; then
  err "规则规范化失败，保持当前版本（主实例未重启）"
  exit 1
fi

staging_hash() {
  ( cd "$1" 2>/dev/null && for f in accept-extra.txt ai-domains.txt apple-cn.txt bogus-ip.txt bogus-nxdomain.china.conf cn-exceptions.txt cn-domains.txt custom-hosts.txt custom-local.txt gfw-domains.txt IPv4.China.list proxy-domains.txt; do
      [ -f "$f" ] || continue
      printf '%s ' "$f"; sha256sum "$f" 2>/dev/null | cut -d' ' -f1
    done ) | sort | sha256sum | cut -d' ' -f1
}

if [ "$FORCE" != 1 ]; then
  cur_set=$(staging_hash "$RULES")
  new_set=$(staging_hash "$STAGING")
  if [ -z "$new_set" ]; then
    warn "staging 为空，忽略"
    exit 0
  fi
  if [ "$cur_set" = "$new_set" ]; then
    log "规则无变化，不重启"
    exit 0
  fi
fi

run_validate() {
  script="$SCRIPT_DIR/validate.sh"
  [ -x "$script" ] || script=/opt/mosdns/validate.sh
  if [ -x "$script" ]; then
    STATE_DIR="$STATE_DIR" LKG_DIR="$LKG_DIR" "$script" --dir "$1" >>"$STATE_DIR/sync.log" 2>&1
    rc=$?
    [ "$rc" = 0 ] || return "$rc"
    if [ -f "$1/custom-hosts.txt" ]; then
      STATE_DIR="$STATE_DIR" LKG_DIR="$LKG_DIR" "$script" --custom-hosts "$1/custom-hosts.txt" >>"$STATE_DIR/sync.log" 2>&1
    fi
  else
    for f in cn-domains.txt IPv4.China.list gfw-domains.txt proxy-domains.txt; do
      [ -s "$1/$f" ] || return 1
    done
  fi
}


dig_ok() {
  port="$1"; name="$2"
  nslookup -port="$port" -timeout=2 "$name" 127.0.0.1 >/dev/null 2>&1
}

request_restart() {
  pid=""
  [ -f "$PID_FILE" ] && pid=$(cat "$PID_FILE")
  [ -z "$pid" ] && pid=$(pgrep -f 'mosdns start' 2>/dev/null | head -1)
  [ -n "$pid" ] || { err "找不到 mosdns 主进程"; return 1; }
  OLD_PID=$pid
  : > "$PID_FILE.reload"
  if ! kill -TERM "$pid" 2>/dev/null; then
    rm -f "$PID_FILE.reload"
    err "SIGTERM $pid 失败"
    return 1
  fi
  log "已向主实例 $pid 发送 SIGTERM（由 entrypoint 拉起）"
}

wait_healthy() {
  timeout_ms=$(( $1 * 1000 ))
  start=$(now_ms)
  restarted=0
  while :; do
    # A healthy response from the old listener is not a post-check. Wait for
    # the supervisor to replace the terminated process before probing DNS.
    if [ -n "${OLD_PID:-}" ]; then
      current=$(cat "$PID_FILE" 2>/dev/null || pgrep -f 'mosdns start' 2>/dev/null | head -1)
      if [ -n "$current" ] && [ "$current" != "$OLD_PID" ]; then restarted=1; fi
    else
      restarted=1
    fi
    if [ "$restarted" = 1 ] && dig_ok "$DIG_PORT" "$CANARY_SENTINEL"; then return 0; fi
    now=$(now_ms)
    [ $((now - start)) -ge "$timeout_ms" ] && return 1
    sleep 1
  done
}

# 金丝雀：用 staging 渲染一份独立配置，在 CANARY_PORT 起临时 mosdns（GS-4）
canary() {
  src="$1"
  cdir="$STATE_DIR/canary"
  rm -rf "$cdir"; mkdir -p "$cdir/rules"
  for f in "$src"/*; do [ -f "$f" ] && cp "$f" "$cdir/rules/"; done
  # Runtime-generated support files (accept-extra, bogus-ip, custom-local)
  # are not part of release-files; preserve active copies unless staged.
  for f in "$RULES"/*; do
    [ -f "$f" ] || continue
    name=$(basename "$f")
    [ -f "$src/$name" ] || cp "$f" "$cdir/rules/" || return 1
  done
  # Check the full candidate set, including staged pins. Using active pins here
  # would allow malformed custom-hosts updates to pass canary.
  if [ -f "$src/custom-hosts.txt" ]; then
    cp "$src/custom-hosts.txt" "$cdir/rules/custom-hosts.txt" || return 1
  elif [ -f "$RULES/custom-hosts.txt" ]; then
    cp "$RULES/custom-hosts.txt" "$cdir/rules/custom-hosts.txt" || return 1
  else
    : > "$cdir/rules/custom-hosts.txt"
  fi
  cp "$CONFIG" "$cdir/config.yaml" 2>/dev/null || { err "没有可用的 config.yaml"; return 1; }
  sed -i \
    -e "s#listen: .*#listen: 127.0.0.1:$CANARY_PORT#" \
    -e "s#http: .*#http: 127.0.0.1:$CANARY_API#" \
    -e "s#/etc/mosdns/rules/#$cdir/rules/#g" \
    -e "s#dump_file: .*#dump_file: $cdir/cache.dump#" \
    "$cdir/config.yaml"
  mosdns start -c "$cdir/config.yaml" >"$cdir/log" 2>&1 &
  cpid=$!
  i=0
  ok=0
  while [ $i -lt 20 ]; do
    if ! kill -0 "$cpid" 2>/dev/null; then
      err "金丝雀进程退出：$(tail -1 "$cdir/log" 2>/dev/null)"
      return 1
    fi
    if dig_ok "$CANARY_PORT" "$CANARY_SENTINEL"; then ok=1; break; fi
    sleep 0.5; i=$((i + 1))
  done
  kill -TERM "$cpid" 2>/dev/null || true
  wait "$cpid" 2>/dev/null || true
  if [ "$ok" != 1 ]; then err "金丝雀无应答（超时）"; return 1; fi
  log "金丝雀通过"
}

snapshot_prev() {
  rm -rf "$PREV_DIR"; mkdir -p "$PREV_DIR"
  for f in "$RULES"/*; do [ -f "$f" ] && cp "$f" "$PREV_DIR/"; done
}

move_staging_into_rules() {
  for f in "$STAGING"/*; do
    [ -f "$f" ] || continue
    name=$(basename "$f")
    case "$name" in .*) continue ;; esac
    if ! cp "$f" "$RULES/$name.new" || ! mv "$RULES/$name.new" "$RULES/$name"; then
      return 1
    fi
  done
}

rollback_to_prev() {
  [ -d "$PREV_DIR" ] || { err "没有可回滚的上一版"; return 1; }
  # Remove candidate-only managed files as well as restoring the prior set.
  for name in cn-domains.txt proxy-domains.txt gfw-domains.txt ai-domains.txt IPv4.China.list apple-cn.txt cn-exceptions.txt bogus-nxdomain.china.conf bogus-ip.txt custom-hosts.txt; do
    if [ -f "$RULES/$name" ] && [ ! -f "$PREV_DIR/$name" ]; then rm -f "$RULES/$name"; fi
  done
  for f in "$PREV_DIR"/*; do
    [ -f "$f" ] || continue
    cp "$f" "$RULES/$(basename "$f").new" && mv "$RULES/$(basename "$f").new" "$RULES/$(basename "$f")" || return 1
  done
  request_restart || true
  wait_healthy "$RESTART_TIMEOUT" || { err "回滚后仍不健康"; return 1; }
  log "已回滚到上一版"
}

# ---------- 主流程 ----------
if ! run_validate "$STAGING"; then
  err "校验失败，保持 LKG（主实例未重启）"
  exit 1
fi


if ! canary "$STAGING"; then
  err "金丝雀失败，主实例未重启"
  exit 1
fi

snapshot_prev
stamp=$(date '+%Y%m%dT%H%M%S')
mkdir -p "$HISTORY_DIR"
if [ -d "$PREV_DIR" ]; then cp -a "$PREV_DIR" "$HISTORY_DIR/$stamp" 2>/dev/null || true; fi
# 历史只保留最近 HISTORY_KEEP 份
ls -1 "$HISTORY_DIR" 2>/dev/null | sort -r | tail -n +$((HISTORY_KEEP + 1)) | while read -r old; do
  rm -rf "$HISTORY_DIR/$old"
done

if ! move_staging_into_rules; then
  err "切换文件失败，恢复上一版"
  rollback_to_prev || err "恢复失败，请人工介入"
  exit 1
fi

t0=$(now_ms)
if ! request_restart; then
  err "重启失败，回滚"
  rollback_to_prev || true
  exit 1
fi

if ! wait_healthy "$RESTART_TIMEOUT"; then
  err "后检失败，自动回滚（GS-5）"
  rollback_to_prev || err "回滚也失败了，请人工介入"
  exit 1
fi
t1=$(now_ms)
echo "$((t1 - t0))" > "$STATE_DIR/last_restart_ms"
if [ -f "$SCRIPT_DIR/runtime.sh" ]; then
  . "$SCRIPT_DIR/runtime.sh"
  RUNTIME_VALIDATE_SCRIPT="$SCRIPT_DIR/validate.sh"
  if ! runtime_promote_lkg "$RULES" "$LKG_DIR"; then
    warn "应用成功但完整 LKG 快照未能发布"
  fi
fi
log "应用完成，重启到可用耗时 $((t1 - t0))ms"
exit 0

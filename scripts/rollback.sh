#!/bin/sh
# rollback.sh — 回滚到历史版本（最近 HISTORY_KEEP 份，默认 20）
#
# 用法：
#   rollback.sh --list            列出可用历史版本
#   rollback.sh [--to 20261109T153000]  回滚到指定版本（默认上一份）
#   rollback.sh --lkg             回滚到 LKG 目录（$STATE_DIR/lkg）
set -u
STATE_DIR="${STATE_DIR:-/data/state}"
RULES="${RULES_DIR:-/etc/mosdns/rules}"
HISTORY_DIR="${HISTORY_DIR:-$STATE_DIR/history}"
LKG_DIR="${LKG_DIR:-$STATE_DIR/lkg}"
PID_FILE="${PID_FILE:-/tmp/mosdns.pid}"
RESTART_TIMEOUT="${RESTART_TIMEOUT:-20}"
SENTINEL="${CANARY_SENTINEL:-www.baidu.com}"

log() { echo "[rollback] $*"; }
err() { echo "[rollback] ERROR $*" >&2; }

TARGET=""
while [ $# -gt 0 ]; do
  case "$1" in
    --list) ls -1 "$HISTORY_DIR" 2>/dev/null | sort -r; exit 0 ;;
    --lkg) TARGET="$LKG_DIR"; shift ;;
    --to) TARGET="$HISTORY_DIR/$2"; shift 2 ;;
    -h|--help) sed -n '2,8p' "$0"; exit 0 ;;
    *) err "未知参数 $1"; exit 2 ;;
  esac
done

if [ -z "$TARGET" ]; then
  TARGET="$HISTORY_DIR/$(ls -1 "$HISTORY_DIR" 2>/dev/null | sort -r | head -1)"
fi
[ -d "$TARGET" ] || { err "目标版本不存在：$TARGET"; exit 2; }

log "回滚到 $TARGET"
for f in "$TARGET"/*; do
  [ -f "$f" ] || continue
  name=$(basename "$f")
  cp "$f" "$RULES/$name.new" && mv "$RULES/$name.new" "$RULES/$name" || { err "复制 $name 失败"; exit 1; }
done

pid=""
[ -f "$PID_FILE" ] && pid=$(cat "$PID_FILE")
[ -z "$pid" ] && pid=$(pgrep -f 'mosdns start' 2>/dev/null | head -1)
[ -n "$pid" ] || { err "找不到 mosdns 主进程"; exit 1; }
kill -TERM "$pid" 2>/dev/null || { err "SIGTERM 失败"; exit 1; }

start=$(date +%s)
while :; do
  if nslookup -timeout=2 "$SENTINEL" 127.0.0.1 >/dev/null 2>&1; then
    log "回滚完成，用时 $(( $(date +%s) - start ))s"
    exit 0
  fi
  [ $(( $(date +%s) - start )) -ge "$RESTART_TIMEOUT" ] && { err "回滚后后检超时"; exit 1; }
  sleep 1
done
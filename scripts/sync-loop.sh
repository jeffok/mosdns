#!/bin/sh
# sync-loop.sh — 常驻同步循环：定时 fetch → validate → apply，失败保持 LKG
#
# 由 entrypoint 在后台启动。要点（方案 §19.4 / GS-9~GS-13）：
#   * 规则每日一次（RULES_UPDATE_CRON 语义用 RULES_UPDATE_INTERVAL 秒表达，默认 86400）
#   * custom-hosts 每 CUSTOM_HOSTS_INTERVAL 秒拉一次；主源不可达保持当前文件，不切到其它源
#   * 每次判完变化才重启；崩溃由 entrypoint 重新拉起，不影响 DNS
set -u
STATE_DIR="${STATE_DIR:-/data/state}"
RULES="${RULES_DIR:-/etc/mosdns/rules}"
SCRIPT_DIR="${SCRIPT_DIR:-/opt/mosdns}"
RULES_UPDATE_INTERVAL="${RULES_UPDATE_INTERVAL:-86400}"
CUSTOM_HOSTS_INTERVAL="${CUSTOM_HOSTS_INTERVAL:-120}"
CUSTOM_HOSTS_SOURCES="${CUSTOM_HOSTS_SOURCES:-https://gitea.jeffok.com:4443/jeff/mosdns/raw/branch/main/rules/custom-hosts.txt}"
LKG_DIR="${LKG_DIR:-$STATE_DIR/lkg}"
STAGING="${STAGING:-$STATE_DIR/staging}"
FETCH_TIMEOUT="${RULES_FETCH_TIMEOUT:-60}"
CUSTOM_HOSTS_PERIOD="${CUSTOM_HOSTS_INTERVAL:-120}"

log() { echo "[$(date '+%H:%M:%S')] [sync] $*"; }
warn() { echo "[$(date '+%H:%M:%S')] [sync] WARN $*" >&2; }

mkdir -p "$STATE_DIR" "$LKG_DIR"

# LKG 维护：任何一次成功应用之后才更新
promote_lkg() {
  for f in cn-domains.txt proxy-domains.txt gfw-domains.txt ai-domains.txt IPv4.China.list; do
    [ -s "$RULES/$f" ] && cp "$RULES/$f" "$LKG_DIR/$f"
  done
}

script() { s="$SCRIPT_DIR/$1"; [ -x "$s" ] || s="/opt/mosdns/$1"; printf '%s' "$s"; }

sync_custom_hosts() {
  [ -n "$CUSTOM_HOSTS_SOURCES" ] || return 0
  url=$(echo "$CUSTOM_HOSTS_SOURCES" | tr ',' '\n' | tr -d ' ' | head -1)
  tmp="$STATE_DIR/custom-hosts.new"
  # 主源不可达：保持当前文件（GS-15）
  if ! wget -q -T "$FETCH_TIMEOUT" -O "$tmp" "$url" 2>/dev/null || [ ! -s "$tmp" ]; then
    warn "custom-hosts 主源不可达，保持当前文件"
    rm -f "$tmp"
    return 1
  fi
  if [ -f "$RULES/custom-hosts.txt" ] && cmp -s "$tmp" "$RULES/custom-hosts.txt"; then
    rm -f "$tmp"
    return 0
  fi
  if ! "$(script validate.sh)" --custom-hosts "$tmp" >>"$STATE_DIR/sync.log" 2>&1; then
    warn "custom-hosts 校验失败，保持当前文件（GS-3）"
    rm -f "$tmp"
    return 1
  fi
  # 金丝雀 + 切换 + 重启交给 apply.sh 的统一路径
  cp "$tmp" "$STAGING/custom-hosts.txt"
  rm -f "$tmp"
  if "$(script apply.sh)" --now >>"$STATE_DIR/sync.log" 2>&1; then
    log "custom-hosts 已更新"
  else
    warn "custom-hosts 应用失败（已回滚或未重启）"
    return 1
  fi
}

sync_rules() {
  if ! "$(script fetch.sh)" >>"$STATE_DIR/sync.log" 2>&1; then
    warn "规则抓取失败，保持 LKG"
    return 1
  fi
  # fetch 已写出 staging；无变化时 apply 会直接退出
  if "$(script apply.sh)" --now >>"$STATE_DIR/sync.log" 2>&1; then
    promote_lkg
    log "规则已应用"
  else
    warn "规则应用失败（校验/金丝雀/后检未通过）"
    return 1
  fi
}

next_rules=$(( $(date +%s) + 30 ))   # 启动 30 秒后先同步一次
while :; do
  now=$(date +%s)
  if [ "$now" -ge "$next_rules" ]; then
    log "开始规则同步"
    sync_rules || true
    next_rules=$(( $(date +%s) + RULES_UPDATE_INTERVAL ))
  fi
  sync_custom_hosts || true
  sleep "$CUSTOM_HOSTS_PERIOD"
done
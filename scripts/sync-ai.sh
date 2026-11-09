#!/bin/sh
# sync-ai.sh — 解析 AI 域名，通过 REST API 写入 RouterOS address-list
#
# 由 entrypoint 在 ENABLE_ROS_SYNC=1 且 ROS_HOST/ROS_PASS 齐全时以后台循环调用。
# 名单只读 mosdns 正在使用的 ai-domains.txt；名单更新走 fetch/validate/apply。
# 本脚本失败只影响本轮同步，不得影响 DNS，不得打印 ROS_PASS 或 Authorization。
set -u

RULES=${RULES_DIR:-/etc/mosdns/rules}
AI_FILE="$RULES/ai-domains.txt"
LIST="${ROS_AI_LIST:-ai-sgp}"
TTL="${ROS_AI_TTL:-1800s}"
COMMENT="${ROS_AI_COMMENT:-mosdns-ai}"
ROS_USER="${ROS_USER:-admin}"
ROS_SCHEME="${ROS_SCHEME:-http}"
CONTAINER_DNS="${CONTAINER_DNS:-119.29.29.29}"
SYNC_DNS_RAW="${SYNC_AI_DNS:-$CONTAINER_DNS}"

log() { echo "[sync-ai] $*"; }
warn() { echo "[sync-ai] WARN $*" >&2; }

# 日志用主机名，绝不带 userinfo
ROS_HOST="${ROS_HOST:-}"
[ -n "$ROS_HOST" ] || { warn "ROS_HOST 为空，跳过本轮"; exit 0; }
[ -n "${ROS_PASS:-}" ] || { warn "ROS_PASS 为空，跳过本轮"; exit 0; }

case "$ROS_SCHEME" in
  http|https) ;;
  *) warn "ROS_SCHEME=$ROS_SCHEME 无效，按 http 处理"; ROS_SCHEME=http ;;
esac

SYNC_HOST=$SYNC_DNS_RAW
SYNC_PORT=53
case "$SYNC_DNS_RAW" in
  *:*)
    SYNC_HOST=${SYNC_DNS_RAW%:*}
    SYNC_PORT=${SYNC_DNS_RAW##*:}
    ;;
esac
[ -n "$SYNC_HOST" ] || SYNC_HOST=119.29.29.29
case "$SYNC_PORT" in
  ''|*[!0-9]*) SYNC_PORT=53 ;;
esac

WGET_INSECURE=""
if [ "$ROS_SCHEME" = https ]; then
  case "${ROS_TLS_INSECURE:-0}" in
    1|true|yes) WGET_INSECURE=--no-check-certificate ;;
  esac
fi

AUTH=$(printf '%s:%s' "$ROS_USER" "$ROS_PASS" | base64 | tr -d '\n')
API="${ROS_SCHEME}://${ROS_HOST}/rest/ip/firewall/address-list"
WGET_ERR=${STATE_DIR:-/tmp}/sync-ai.wget.err

api_get() {
  wget -q -O- $WGET_INSECURE --header "Authorization: Basic $AUTH" "$1" 2>"$WGET_ERR" || true
}

api_post() {
  wget -q -O- $WGET_INSECURE \
    --header "Authorization: Basic $AUTH" \
    --header "content-type: application/json" \
    --post-data "$2" "$1" 2>"$WGET_ERR" || true
}

wget_is_unauthorized() {
  grep -q '401 Unauthorized' "$WGET_ERR" 2>/dev/null
}

is_valid_ipv4() {
  case "$1" in ""|*[!0-9.]*) return 1 ;; esac
  o1=$(echo "$1" | cut -d. -f1); o2=$(echo "$1" | cut -d. -f2)
  o3=$(echo "$1" | cut -d. -f3); o4=$(echo "$1" | cut -d. -f4)
  [ -n "$o1" ] && [ -n "$o2" ] && [ -n "$o3" ] && [ -n "$o4" ] || return 1
  for o in "$o1" "$o2" "$o3" "$o4"; do [ "$o" -ge 0 ] && [ "$o" -le 255 ] || return 1; done
  [ "$o1" -eq 0 ] || [ "$o1" -eq 127 ] && return 1
  [ "$o1" -eq 169 ] && [ "$o2" -eq 254 ] && return 1
  return 0
}

resolve_ipv4s() {
  domain="$1"
  if [ "$SYNC_PORT" = 53 ]; then
    nslookup "$domain" "$SYNC_HOST" 2>/dev/null | awk '
      $1 == "Name:" { seen_name = 1; next }
      seen_name && $1 == "Address:" { print $2; next }
      seen_name && $1 == "Address" && $2 ~ /^[0-9]+:$/ { print $3; next }
    ' | grep -E '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$' | sort -u
  elif command -v dig >/dev/null 2>&1; then
    dig @"$SYNC_HOST" -p "$SYNC_PORT" "$domain" A +short +time=2 +tries=1 2>/dev/null \
      | grep -E '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$' | sort -u
  else
    warn "SYNC_AI_DNS 指定了非 53 端口 $SYNC_PORT，但镜像内无 dig，跳过 $domain"
  fi
}

normalize_domain() {
  line=$1
  line=$(printf '%s' "$line" | sed 's/#.*//' | tr -d '\r')
  line=$(printf '%s' "$line" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
  [ -n "$line" ] || return 1
  case "$line" in
    keyword:*|regexp:*) return 1 ;;
    domain:*|full:*) line=${line#*:} ;;
  esac
  [ -n "$line" ] || return 1
  printf '%s\n' "$line"
}

[ -f "$AI_FILE" ] || { warn "缺少 $AI_FILE，跳过本轮（名单更新请走 fetch/validate/apply）"; exit 0; }

probe=$(api_get "${API}?list=${LIST}&comment=${COMMENT}")
if wget_is_unauthorized || printf '%s' "$probe" | grep -q '401'; then
  warn "RouterOS 认证失败，本轮跳过（DNS 不受影响）"
  exit 0
fi
if [ -z "$probe" ]; then
  warn "RouterOS REST 不可达或无响应，本轮跳过（DNS 不受影响）"
  exit 0
fi

OLD_IDS=$(printf '%s' "$probe" | grep -o '"\.id":"[^"]*"' | cut -d'"' -f4)
DEL_COUNT=0
for id in $OLD_IDS; do
  [ -n "$id" ] || continue
  api_post "${API}/remove" "{\".id\":\"${id}\"}" >/dev/null
  DEL_COUNT=$((DEL_COUNT + 1))
done

DOMAIN_COUNT=0
IP_COUNT=0
ADD_OK=0
ADD_FAIL=0

while IFS= read -r raw || [ -n "$raw" ]; do
  domain=$(normalize_domain "$raw") || continue
  DOMAIN_COUNT=$((DOMAIN_COUNT + 1))
  for ip in $(resolve_ipv4s "$domain"); do
    if is_valid_ipv4 "$ip"; then
      IP_COUNT=$((IP_COUNT + 1))
      resp=$(api_post "${API}/add" "{\"list\":\"${LIST}\",\"address\":\"${ip}\",\"timeout\":\"${TTL}\",\"comment\":\"${COMMENT}\"}")
      case "$resp" in
        *'"ret"'*) ADD_OK=$((ADD_OK + 1)) ;;
        *) ADD_FAIL=$((ADD_FAIL + 1)) ;;
      esac
    fi
  done
done < "$AI_FILE"

if [ "$ADD_OK" -gt 0 ]; then
  log "ok domains=$DOMAIN_COUNT ips=$IP_COUNT added=$ADD_OK failed=$ADD_FAIL deleted=$DEL_COUNT"
else
  warn "domains=$DOMAIN_COUNT ips=$IP_COUNT added=0 failed=$ADD_FAIL deleted=$DEL_COUNT"
fi

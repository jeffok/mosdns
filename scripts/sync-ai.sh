#!/bin/sh
# sync-ai.sh — 解析 AI 域名，通过 REST API 写入 RouterOS ai-sgp address-list
RULES=/etc/mosdns/rules
AI_LIST="$RULES/ai-list.txt"
AI_LIST_URL="${AI_LIST_URL:-https://raw.githubusercontent.com/jeffok/mosdns/main/rules/ai-list.txt|https://gh-proxy.com/https://raw.githubusercontent.com/jeffok/mosdns/main/rules/ai-list.txt}"
LIST="ai-sgp"
COMMENT="mosdns-ai"
TTL="1800s"
# 优先使用 CONTAINER_DNS（容器内部专用 DNS），确保 AI 域名解析不受 DNS_GLOBAL 影响
DNS="${CONTAINER_DNS:-8.8.8.8}"
PIDFILE="${PIDFILE:-/tmp/mosdns.pid}"
RELOAD_ON_AI_LIST_CHANGE="${RELOAD_ON_AI_LIST_CHANGE:-1}"
AI_LIST_CHANGED=0

refresh_ai_list() {
  [ -z "$AI_LIST_URL" ] && return 0
  tmp="${AI_LIST}.tmp"

  downloaded=0

  # 安全解析多源链接
  old_ifs="$IFS"
  IFS='|'
  set -- $AI_LIST_URL
  IFS="$old_ifs"

  for url; do
    url=$(echo "$url" | xargs)
    [ -z "$url" ] && continue
    
    # 一旦下载成功立即 break
    if wget -q --timeout=10 -O "$tmp" "$url" && [ -s "$tmp" ]; then
      if [ -f "$AI_LIST" ] && cmp -s "$AI_LIST" "$tmp"; then
        rm -f "$tmp"
      else
        mv "$tmp" "$AI_LIST"
        AI_LIST_CHANGED=1
        echo "[sync-ai] refreshed ai-list from $url (changed)"
      fi
      downloaded=1
      break
    fi
    rm -f "$tmp"
  done

  if [ "$downloaded" = "0" ]; then
    if [ -s "$AI_LIST" ]; then
      echo "[sync-ai] WARN: refresh failed, using local ai-list"
      return 0
    fi
    echo "[sync-ai] WARN: ai-list missing and remote refresh failed"
    return 1
  fi
}

request_mosdns_reload() {
  [ "$RELOAD_ON_AI_LIST_CHANGE" = "0" ] && return 0
  [ "$RELOAD_ON_AI_LIST_CHANGE" = "false" ] && return 0
  if [ -s "$PIDFILE" ]; then
    pid="$(cat "$PIDFILE" 2>/dev/null)"
    if [ -n "$pid" ] && kill "$pid" 2>/dev/null; then
      echo "[sync-ai] ai-list changed, requested mosdns reload (pid=$pid)"
      return 0
    fi
  fi
  echo "[sync-ai] WARN: ai-list changed, but mosdns pid not found"
}

resolve_ipv4s() {
  domain="$1"
  nslookup "$domain" "$DNS" 2>/dev/null | awk '
    $1 == "Name:" { seen_name = 1; next }
    seen_name && $1 == "Address:" { print $2; next }
    seen_name && $1 == "Address" && $2 ~ /^[0-9]+:$/ { print $3; next }
  ' | grep -E '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$' | sort -u
}

[ -d "$RULES" ] || mkdir -p "$RULES"
refresh_ai_list || exit 0
[ "$AI_LIST_CHANGED" -eq 1 ] && request_mosdns_reload

[ -z "$ROS_HOST" ] && exit 0
[ -z "$ROS_PASS" ] && exit 0
[ ! -f "$AI_LIST" ] && exit 0

ROS_USER="${ROS_USER:-admin}"
AUTH=$(printf '%s:%s' "$ROS_USER" "$ROS_PASS" | base64)
API="http://${ROS_HOST}/rest/ip/firewall/address-list"

api_get() {
  wget -q -O- --header "Authorization: Basic $AUTH" "$1" 2>/dev/null
}

api_post() {
  wget -q -O- --header "Authorization: Basic $AUTH" \
    --header "content-type: application/json" \
    --post-data "$2" "$1" 2>/dev/null
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

# ---- 1. 清理旧条目 ----
OLD_IDS=$(api_get "${API}?list=${LIST}&comment=${COMMENT}" \
  | grep -o '"\.id":"[^"]*"' | cut -d'"' -f4)
DEL_COUNT=0
for id in $OLD_IDS; do
  api_post "${API}/remove" "{\".id\":\"${id}\"}" >/dev/null && DEL_COUNT=$((DEL_COUNT + 1))
done

# ---- 2. 解析域名 ----
DOMAIN_COUNT=0
IP_COUNT=0
ADD_OK=0
ADD_FAIL=0

while IFS= read -r line; do
  domain=$(echo "$line" | sed 's/#.*//' | xargs)
  [ -z "$domain" ] && continue
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
done < "$AI_LIST"

if [ "$ADD_OK" -gt 0 ]; then
  echo "[sync-ai] ok domains=$DOMAIN_COUNT ips=$IP_COUNT added=$ADD_OK failed=$ADD_FAIL deleted=$DEL_COUNT"
else
  echo "[sync-ai] WARN: domains=$DOMAIN_COUNT ips=$IP_COUNT added=0 failed=$ADD_FAIL deleted=$DEL_COUNT"
fi

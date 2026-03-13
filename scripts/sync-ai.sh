#!/bin/sh
# sync-ai.sh — 解析 AI 域名，通过 REST API 写入 RouterOS ai-sgp address-list
RULES=/etc/mosdns/rules
AI_LIST="$RULES/ai-list.txt"
LIST="ai-sgp"
COMMENT="mosdns-ai"
TTL="1800s"
_GL_FIRST=$(echo "${DNS_GLOBAL:-}" | cut -d',' -f1 | xargs)
DNS="${DNS_SERVER:-${_GL_FIRST:-10.100.89.3}}"

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

  for ip in $(nslookup "$domain" "$DNS" 2>/dev/null | awk '/^Address:/{print $2}' | grep -v ":"); do
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

#!/bin/sh
RULES=/etc/mosdns/rules
AI_LIST="$RULES/ai-list.txt"
LIST="ai-sgp"
COMMENT="mosdns-ai"
TTL="1800s"
DNS="${DNS_SERVER:-10.100.89.3}"

[ -z "$ROS_HOST" ] || [ -z "$ROS_PASS" ] || [ ! -f "$AI_LIST" ] && exit 0

is_valid_ipv4() {
  ip="$1"

  case "$ip" in
    ""|*[!0-9.]*) return 1 ;;
  esac

  o1=$(echo "$ip" | cut -d. -f1)
  o2=$(echo "$ip" | cut -d. -f2)
  o3=$(echo "$ip" | cut -d. -f3)
  o4=$(echo "$ip" | cut -d. -f4)

  [ -n "$o1" ] && [ -n "$o2" ] && [ -n "$o3" ] && [ -n "$o4" ] || return 1
  [ "$o1" -ge 0 ] && [ "$o1" -le 255 ] || return 1
  [ "$o2" -ge 0 ] && [ "$o2" -le 255 ] || return 1
  [ "$o3" -ge 0 ] && [ "$o3" -le 255 ] || return 1
  [ "$o4" -ge 0 ] && [ "$o4" -le 255 ] || return 1

  # 过滤明显无效/本机链路地址
  [ "$o1" -eq 0 ] && return 1
  [ "$o1" -eq 127 ] && return 1
  [ "$o1" -eq 169 ] && [ "$o2" -eq 254 ] && return 1

  return 0
}

CMD=""
DOMAIN_COUNT=0
IP_COUNT=0

while IFS= read -r line; do
  domain=$(echo "$line" | sed 's/#.*//' | xargs)
  [ -z "$domain" ] && continue

  DOMAIN_COUNT=$((DOMAIN_COUNT + 1))

  # 保持 nslookup，不新增 dig 等工具
  for ip in $(nslookup "$domain" "$DNS" 2>/dev/null | awk '/^Address:/{print $2}' | grep -v ":"); do
    if is_valid_ipv4 "$ip"; then
      CMD="$CMD /ip/firewall/address-list/add list=$LIST address=$ip timeout=$TTL comment=$COMMENT;"
      IP_COUNT=$((IP_COUNT + 1))
    fi
  done
done < "$AI_LIST"

[ -z "$CMD" ] && {
  echo "[sync-ai] no valid ipv4 resolved from $DOMAIN_COUNT domains"
  exit 0
}

ROS_ADDR="${ROS_HOST%:*}"
ROS_PORT="${ROS_HOST##*:}"
[ "$ROS_PORT" = "$ROS_ADDR" ] && ROS_PORT=6220

sshpass -p "$ROS_PASS" ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 \
  -p "$ROS_PORT" admin@"$ROS_ADDR" "$CMD" 2>/dev/null \
  && echo "[sync-ai] domains=$DOMAIN_COUNT ips=$IP_COUNT -> $ROS_HOST" \
  || echo "[sync-ai] WARN: failed (domains=$DOMAIN_COUNT ips=$IP_COUNT host=$ROS_HOST)"

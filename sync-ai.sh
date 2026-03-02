#!/bin/sh
RULES=/etc/mosdns/rules
AI_LIST="$RULES/ai-list.txt"
LIST="ai-sgp"
COMMENT="mosdns-ai"
TTL="1800s"
DNS="${DNS_SERVER:-10.100.89.3}"

[ -z "$ROS_HOST" ] || [ -z "$ROS_PASS" ] || [ ! -f "$AI_LIST" ] && exit 0

CMD=""
COUNT=0
while IFS= read -r line; do
  domain=$(echo "$line" | sed 's/#.*//' | xargs)
  [ -z "$domain" ] && continue
  for ip in $(nslookup "$domain" "$DNS" 2>/dev/null \
    | awk '/^Address:/{if(NR>2)print $2}' | grep -v ":"); do
    CMD="$CMD /ip/firewall/address-list/add list=$LIST address=$ip timeout=$TTL comment=$COMMENT;"
    COUNT=$((COUNT+1))
  done
done < "$AI_LIST"

[ -z "$CMD" ] && exit 0

ROS_ADDR="${ROS_HOST%:*}"
ROS_PORT="${ROS_HOST##*:}"
[ "$ROS_PORT" = "$ROS_ADDR" ] && ROS_PORT=6220

sshpass -p "$ROS_PASS" ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 \
  -p "$ROS_PORT" admin@"$ROS_ADDR" "$CMD" 2>/dev/null \
  && echo "[sync-ai] $COUNT ips -> $ROS_HOST" \
  || echo "[sync-ai] WARN: failed"

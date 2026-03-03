#!/bin/sh
RULES=/etc/mosdns/rules
PIDFILE=/tmp/mosdns.pid
mkdir -p "$RULES"

log() { echo "[entrypoint] $*"; }

# 规则下载源（名称|URL）
RULE_SOURCES="
direct-list.txt|https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/direct-list.txt
china_ip_list.txt|https://raw.githubusercontent.com/Loyalsoldier/geoip/refs/heads/release/text/cn.txt
apple-cn.txt|https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/apple-cn.txt
proxy-list.txt|https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/proxy-list.txt
geosite-gfw.txt|https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/gfw.txt
"

# ROS 容器不会自动注入 DNS
if [ -n "$CONTAINER_DNS" ]; then
  echo "nameserver $CONTAINER_DNS" > /etc/resolv.conf
  log "resolv.conf <- $CONTAINER_DNS"
fi

# ---- 从环境变量生成 config.yaml ----
DNS_CN="${DNS_CN:-119.29.29.29,223.5.5.5,114.114.114.114}"
DNS_GLOBAL="${DNS_GLOBAL:-1.1.1.1,8.8.8.8,9.9.9.9}"
DNS_AI="${DNS_AI:-$DNS_GLOBAL}"

dns_to_yaml() {
  echo "$1" | tr ',' '\n' | while read -r addr; do
    addr=$(echo "$addr" | xargs)
    [ -n "$addr" ] && printf "    - addr: %s\n" "$addr"
  done
}

count_dns() {
  n=$(echo "$1" | tr ',' '\n' | grep -c .)
  [ "$n" -lt 1 ] && n=1
  echo "$n"
}

TEMPLATE=/opt/mosdns/config.template.yaml
if [ -f "$TEMPLATE" ]; then
  sed -e "s|__CONCURRENT_CN__|$(count_dns "$DNS_CN")|" \
      -e "s|__CONCURRENT_GLOBAL__|$(count_dns "$DNS_GLOBAL")|" \
      -e "s|__CONCURRENT_AI__|$(count_dns "$DNS_AI")|" \
      "$TEMPLATE" > /tmp/config.tmp

  CN_YAML=$(dns_to_yaml "$DNS_CN")
  GL_YAML=$(dns_to_yaml "$DNS_GLOBAL")
  AI_YAML=$(dns_to_yaml "$DNS_AI")

  awk -v cn="$CN_YAML" -v gl="$GL_YAML" -v ai="$AI_YAML" \
    '/__UPSTREAMS_CN__/{print cn;next}
     /__UPSTREAMS_GLOBAL__/{print gl;next}
     /__UPSTREAMS_AI__/{print ai;next}
     {print}' /tmp/config.tmp > /etc/mosdns/config.yaml
  rm -f /tmp/config.tmp

  case "$DOH_ENABLED" in 1|true|yes)
    cat >> /etc/mosdns/config.yaml <<DOEOF
- tag: doh_server
  type: http_server
  args:
    entries:
    - path: /dns-query
      exec: main_sequence
    listen: 0.0.0.0:8443
    cert: ${DOH_CERT:-/etc/mosdns/certs/fullchain.pem}
    key: ${DOH_KEY:-/etc/mosdns/certs/privkey.pem}
    idle_timeout: 10
DOEOF
    log "DoH enabled on :8443"
  ;; esac

  log "config.yaml generated: CN=$(count_dns "$DNS_CN") GLOBAL=$(count_dns "$DNS_GLOBAL") AI=$(count_dns "$DNS_AI")"
fi

# ---- 复制镜像内置规则文件 ----
if [ -d /opt/mosdns-rules ]; then
  for f in /opt/mosdns-rules/*; do [ -f "$f" ] && cp "$f" "$RULES/"; done
fi

# ROS 容器 veth 启动后需要等一会儿才能连外网
wait_for_network() {
  max_wait=180; elapsed=0
  while [ $elapsed -lt $max_wait ]; do
    nslookup raw.githubusercontent.com >/dev/null 2>&1 && { log "network ready (${elapsed}s)"; return 0; }
    sleep 5; elapsed=$((elapsed + 5))
  done
  log "WARN network not ready after ${max_wait}s"
  return 1
}

ensure_rule_files() {
  echo "$RULE_SOURCES" | while IFS='|' read -r name url; do
    name=$(echo "$name" | xargs)
    [ -z "$name" ] && continue
    [ -f "$RULES/$name" ] || { touch "$RULES/$name"; log "created empty fallback $name"; }
  done
}

dl_rules() {
  echo "$RULE_SOURCES" | while IFS='|' read -r name url; do
    name=$(echo "$name" | xargs); url=$(echo "$url" | xargs)
    [ -z "$name" ] && continue
    tmp="$RULES/${name}.tmp"
    if wget -q -O "$tmp" "$url" && [ -s "$tmp" ]; then
      mv "$tmp" "$RULES/$name"; log "ok $name"
    else
      rm -f "$tmp"; log "WARN $name download failed"
    fi
  done
}

# ---- crontab: 04:30 杀 mosdns 触发规则重载；每 2 分钟 sync-ai ----
RELOAD_DELAY="${RELOAD_DELAY:-0}"
touch /etc/mosdns/cache.dump
{
  printf "30 4 * * * sleep %s; kill \$(cat %s 2>/dev/null) 2>/dev/null\n" "$RELOAD_DELAY" "$PIDFILE"
  printf "*/2 * * * * /sync-ai.sh >/dev/null 2>&1\n"
} | crontab -
crond -b -l 2
log "crond started $(cat /etc/timezone 2>/dev/null || echo UTC)"

# ---- 启动时执行一次 AI 同步 ----
/sync-ai.sh

# ---- 循环：拉规则 -> 启动 mosdns -> 等待 ----
FIRST_RUN=1
while true; do
  if [ "$FIRST_RUN" = 1 ]; then
    wait_for_network
    ensure_rule_files
    FIRST_RUN=0
  fi

  dl_rules

  log "starting mosdns"
  mosdns start -c /etc/mosdns/config.yaml &
  echo $! > "$PIDFILE"
  wait "$(cat "$PIDFILE")"
  log "mosdns exited, reloading"
done

#!/bin/sh
RULES=/etc/mosdns/rules
PIDFILE=/tmp/mosdns.pid
mkdir -p "$RULES"

log() { echo "[entrypoint] $*"; }

# 规则下载源（名称|URL1|URL2|... 多源按序尝试）
RULE_SOURCES="
direct-list.txt|https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/direct-list.txt|https://gh-proxy.com/https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/direct-list.txt|https://mirror.ghproxy.com/https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/direct-list.txt
china_ip_list.txt|https://raw.githubusercontent.com/Loyalsoldier/geoip/refs/heads/release/text/cn.txt|https://gh-proxy.com/https://raw.githubusercontent.com/Loyalsoldier/geoip/refs/heads/release/text/cn.txt|https://mirror.ghproxy.com/https://raw.githubusercontent.com/Loyalsoldier/geoip/refs/heads/release/text/cn.txt
apple-cn.txt|https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/apple-cn.txt|https://gh-proxy.com/https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/apple-cn.txt|https://mirror.ghproxy.com/https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/apple-cn.txt
proxy-list.txt|https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/proxy-list.txt|https://gh-proxy.com/https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/proxy-list.txt|https://mirror.ghproxy.com/https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/proxy-list.txt
geosite-gfw.txt|https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/gfw.txt|https://gh-proxy.com/https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/gfw.txt|https://mirror.ghproxy.com/https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/gfw.txt
"

# ROS 容器不会自动注入 DNS，必须使用可靠的公共 DNS 以保证规则下载成功
# 默认使用 8.8.8.8，确保即使 DNS_GLOBAL 也是国内 IP 也能正常拉取规则
if [ -z "$CONTAINER_DNS" ]; then
  CONTAINER_DNS="8.8.8.8"
fi

echo "nameserver $CONTAINER_DNS" > /etc/resolv.conf
log "set resolv.conf to $CONTAINER_DNS"

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
  ECS_PRESET="${ECS_PRESET:-}"
  sed -e "s|__CONCURRENT_CN__|$(count_dns "$DNS_CN")|" \
      -e "s|__CONCURRENT_GLOBAL__|$(count_dns "$DNS_GLOBAL")|" \
      -e "s|__CONCURRENT_AI__|$(count_dns "$DNS_AI")|" \
      -e "s|__ECS_PRESET__|$ECS_PRESET|" \
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
    _cert="${DOH_CERT:-/etc/mosdns/certs/fullchain.pem}"
    _key="${DOH_KEY:-/etc/mosdns/certs/privkey.pem}"
    if [ -f "$_cert" ] && [ -f "$_key" ]; then
      cat >> /etc/mosdns/config.yaml <<DOEOF
- tag: doh_server
  type: http_server
  args:
    entries:
    - path: /dns-query
      exec: main_sequence
    listen: 0.0.0.0:8443
    cert: $_cert
    key: $_key
    idle_timeout: 10
DOEOF
      log "DoH enabled on :8443"
    else
      log "WARN DOH_ENABLED=1 but cert/key missing ($_cert, $_key), DoH skipped"
    fi
  ;; esac

  log "config.yaml generated: CN=$(count_dns "$DNS_CN") GLOBAL=$(count_dns "$DNS_GLOBAL") AI=$(count_dns "$DNS_AI") ECS_PRESET=${ECS_PRESET:-<empty>}"
fi

# ---- 复制镜像内置规则文件（仅当挂载目录为空或文件缺失时填充）----
if [ -d /opt/mosdns-rules ]; then
  for f in /opt/mosdns-rules/*; do
    if [ -f "$f" ]; then
      dest="$RULES/$(basename "$f")"
      if [ ! -f "$dest" ]; then
        cp "$f" "$dest"
        log "initialized missing $(basename "$f") from image"
      fi
    fi
  done
fi

# 网络可用性检查（依赖系统解析，已在脚本开头设置）
wait_for_network() {
  max_wait=60; elapsed=0
  while [ $elapsed -lt $max_wait ]; do
    nslookup raw.githubusercontent.com >/dev/null 2>&1 && { log "network ready (${elapsed}s)"; return 0; }
    sleep 3; elapsed=$((elapsed + 3))
  done
  log "WARN network not ready after ${max_wait}s, will proceed anyway"
  return 0
}

# 检查规则文件是否过期（秒），未设置或0表示每次都下载
RULE_FILE_MAX_AGE="${RULE_FILE_MAX_AGE:-82800}"

ensure_rule_files() {
  echo "$RULE_SOURCES" | while IFS='|' read -r name urls; do
    name=$(echo "$name" | xargs)
    [ -z "$name" ] && continue
    [ -f "$RULES/$name" ] || { touch "$RULES/$name"; log "created empty fallback $name"; }
  done
}

is_file_expired() {
  [ "$RULE_FILE_MAX_AGE" = "0" ] && return 0
  [ ! -f "$1" ] && return 0
  # 使用 find -mmin 判断是否过期，兼容 BusyBox (Alpine)
  minutes=$((RULE_FILE_MAX_AGE / 60))
  if find "$1" -maxdepth 0 -mmin -$minutes | grep -q .; then
    return 1 # File is recent (not expired)
  fi
  return 0 # Expired
}

dl_rules() {
  echo "$RULE_SOURCES" | while IFS='|' read -r name url_and_mirrors; do
    name=$(echo "$name" | xargs)
    [ -z "$name" ] && continue

    local_file="$RULES/$name"
    tmp="$RULES/${name}.tmp"

    # 检查是否需要更新
    if [ -f "$local_file" ] && [ -s "$local_file" ]; then
      if ! is_file_expired "$local_file"; then
        continue
      fi
    fi

    downloaded=0

    # 安全解析多源链接，避免 IFS 切换带来的副作用
    old_ifs="$IFS"
    IFS='|'
    set -- $url_and_mirrors
    IFS="$old_ifs"

    for url; do
      url=$(echo "$url" | xargs)
      [ -z "$url" ] && continue
      
      # 一旦下载成功立即 break，不会尝试后续镜像源
      if wget -q --timeout=10 -O "$tmp" "$url" && [ -s "$tmp" ]; then
        mv "$tmp" "$local_file"
        log "ok $name from $url"
        downloaded=1
        break
      fi
      rm -f "$tmp"
    done

    # 只有所有源都失败且本地没有文件时，才打印警告
    if [ "$downloaded" = "0" ] && [ ! -s "$local_file" ]; then
      log "WARN $name all sources failed and no local copy"
    fi
  done
}

# ---- crontab: 04:30 杀 mosdns 触发规则重载；每 2 分钟 sync-ai ----
RELOAD_DELAY="${RELOAD_DELAY:-0}"
touch /etc/mosdns/cache.dump
{
  printf "CONTAINER_DNS=%s\n" "$CONTAINER_DNS"
  printf "RELOAD_DELAY=%s\n" "$RELOAD_DELAY"
  printf "30 4 * * * sleep %s; kill \$(cat %s 2>/dev/null) 2>/dev/null\n" "$RELOAD_DELAY" "$PIDFILE"
  cat <<'CRONAI'
*/2 * * * * /sync-ai.sh >/dev/null 2>&1
CRONAI
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

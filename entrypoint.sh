#!/bin/sh
RULES=/etc/mosdns/rules
PIDFILE=/tmp/mosdns.pid
mkdir -p "$RULES"

log() {
  echo "[entrypoint] $*"
}

# 若宿主机未注入 DNS（如 ROS 容器内 wget 无法解析），通过 CONTAINER_DNS 写入 resolv.conf
if [ -n "$CONTAINER_DNS" ]; then
  echo "nameserver $CONTAINER_DNS" > /etc/resolv.conf
  log "resolv.conf <- $CONTAINER_DNS"
fi

# 根据 SITE、DOH_ENABLED 选用配置；DoH 开启（1/true/yes）时用 <site>-doh.yaml 并替换证书路径
SITE=${SITE:-sz}
case "$SITE" in hk|sz|dxb) ;; *) SITE=sz;; esac
case "$DOH_ENABLED" in 1|true|yes) CONFIG_FILE="${SITE}-doh.yaml" ;; *) CONFIG_FILE="${SITE}.yaml" ;; esac
if [ -f "/opt/mosdns-configs/${CONFIG_FILE}" ]; then
  cp "/opt/mosdns-configs/${CONFIG_FILE}" /etc/mosdns/config.yaml
  case "$CONFIG_FILE" in
    *-doh.yaml)
      sed -i "s|__DOH_CERT__|${DOH_CERT:-/etc/mosdns/certs/fullchain.pem}|g; s|__DOH_KEY__|${DOH_KEY:-/etc/mosdns/certs/privkey.pem}|g" /etc/mosdns/config.yaml
    ;;
  esac
  log "SITE=$SITE DOH=$DOH_ENABLED -> config.yaml"
fi

if [ -d /opt/mosdns-rules ]; then
  for f in /opt/mosdns-rules/*; do [ -f "$f" ] && cp "$f" "$RULES/"; done
fi

# RouterOS 容器 veth 网络需要 ~3 分钟建立出站连通性，等待 DNS 可用再下载
wait_for_network() {
  max_wait=180; elapsed=0
  while [ $elapsed -lt $max_wait ]; do
    if nslookup raw.githubusercontent.com >/dev/null 2>&1; then
      log "network ready (${elapsed}s)"
      return 0
    fi
    sleep 5; elapsed=$((elapsed + 5))
  done
  log "WARN network not ready after ${max_wait}s, proceeding with fallback"
  return 1
}

# 确保 mosdns 必需的规则文件存在（空文件也可被加载，仅无分流效果）
REQUIRED_RULES="direct-list.txt china_ip_list.txt apple-cn.txt proxy-list.txt geosite-gfw.txt"
ensure_rule_files() {
  for f in $REQUIRED_RULES; do
    [ -f "$RULES/$f" ] || { touch "$RULES/$f"; log "created empty fallback $f"; }
  done
}

dl() {
  name="$1"
  url="$2"
  tmp="$RULES/${name}.tmp"
  dst="$RULES/$name"

  if wget -q -O "$tmp" "$url" && [ -s "$tmp" ]; then
    mv "$tmp" "$dst"
    log "ok $name"
  else
    rm -f "$tmp"
    log "WARN $name download failed, keep old file"
  fi
}

# --- 1. 设置 crontab：04:30 杀 mosdns 进程（由下方循环重新拉规则并启动）；每 2 分钟 sync-ai ---
# 04:30 加站点固定延迟，减少多站点同时更新带来的上游抖动（保持 /bin/sh 兼容）
case "$SITE" in
  hk) RELOAD_DELAY=20 ;;
  sz) RELOAD_DELAY=40 ;;
  dxb) RELOAD_DELAY=60 ;;
  *) RELOAD_DELAY=0 ;;
esac

touch /etc/mosdns/cache.dump
{
  printf "30 4 * * * sleep %s; kill \$(cat %s 2>/dev/null) 2>/dev/null\n" "$RELOAD_DELAY" "$PIDFILE"
  printf "*/2 * * * * /sync-ai.sh >/dev/null 2>&1\n"
} | crontab -
crond -b -l 2
log "crond started $(cat /etc/timezone 2>/dev/null || echo UTC)"

# --- 2. 启动时执行一次 AI 同步 ---
/sync-ai.sh

# --- 3. 循环：拉规则 → 启动 mosdns → 等待；mosdns 退出后（被 crond kill 或崩溃）再次拉规则并启动 ---
FIRST_RUN=1
while true; do
  if [ "$FIRST_RUN" = 1 ]; then
    wait_for_network
    ensure_rule_files
    FIRST_RUN=0
  fi

  dl direct-list.txt   "https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/direct-list.txt"
  dl china_ip_list.txt "https://raw.githubusercontent.com/Loyalsoldier/geoip/refs/heads/release/text/cn.txt"
  dl apple-cn.txt      "https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/apple-cn.txt"
  dl proxy-list.txt    "https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/proxy-list.txt"
  dl geosite-gfw.txt   "https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/gfw.txt"

  log "starting mosdns"
  mosdns start -c /etc/mosdns/config.yaml &
  echo $! > "$PIDFILE"
  wait "$(cat "$PIDFILE")"
  log "mosdns exited, reloading rules and restarting"
done

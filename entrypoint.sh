#!/bin/sh
RULES=/etc/mosdns/rules
PIDFILE=/tmp/mosdns.pid
mkdir -p "$RULES"

dl() { wget -q -O "$RULES/$1" "$2" && echo "[ok] $1" || echo "[WARN] $1"; }

# --- 1. 设置 crontab：04:30 杀 mosdns 进程（由下方循环重新拉规则并启动）；每 2 分钟 sync-ai ---
touch /etc/mosdns/cache.dump
{
  printf "30 4 * * * kill \$(cat %s 2>/dev/null) 2>/dev/null\n" "$PIDFILE"
  printf "*/2 * * * * /sync-ai.sh >/dev/null 2>&1\n"
} | crontab -
crond -b -l 2
echo "[entrypoint] crond started $(cat /etc/timezone 2>/dev/null || echo UTC)"

# --- 2. 启动时执行一次 AI 同步 ---
/sync-ai.sh

# --- 3. 循环：拉规则 → 启动 mosdns → 等待；mosdns 退出后（被 crond kill 或崩溃）再次拉规则并启动 ---
while true; do
  dl direct-list.txt   "https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/direct-list.txt"
  dl china_ip_list.txt "https://raw.githubusercontent.com/Loyalsoldier/geoip/refs/heads/release/text/cn.txt"
  dl apple-cn.txt      "https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/apple-cn.txt"
  dl proxy-list.txt    "https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/proxy-list.txt"
  dl geosite-gfw.txt   "https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/gfw.txt"
  mosdns start -c /etc/mosdns/config.yaml &
  echo $! > "$PIDFILE"
  wait $(cat "$PIDFILE")
  echo "[entrypoint] mosdns exited, reloading rules and restarting..."
done

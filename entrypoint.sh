#!/bin/sh
RULES=/etc/mosdns/rules
mkdir -p "$RULES"

# --- 1. 下载外部规则文件 ---
dl() { wget -q -O "$RULES/$1" "$2" && echo "[ok] $1" || echo "[WARN] $1"; }
dl direct-list.txt   "https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/direct-list.txt"
dl china_ip_list.txt "https://raw.githubusercontent.com/Loyalsoldier/geoip/refs/heads/release/text/cn.txt"
dl apple-cn.txt      "https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/apple-cn.txt"
dl proxy-list.txt    "https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/proxy-list.txt"
dl geosite-gfw.txt   "https://raw.githubusercontent.com/Loyalsoldier/v2ray-rules-dat/release/gfw.txt"

# --- 2. 启动时执行一次 AI address-list 同步 ---
/sync-ai.sh

# --- 3. 设置 crontab ---
touch /etc/mosdns/cache.dump
{
  printf "30 4 * * * kill 1\n"
  printf "*/2 * * * * /sync-ai.sh >/dev/null 2>&1\n"
} | crontab -
crond -b -l 2
echo "[entrypoint] crond started $(cat /etc/timezone 2>/dev/null || echo UTC)"

exec mosdns start -c /etc/mosdns/config.yaml

#!/bin/sh
# status.sh — 输出当前规则状态：选用的源、各文件条数、年龄、LKG、上次结果
set -u
STATE_DIR="${STATE_DIR:-/data/state}"
RULES="${RULES_DIR:-/etc/mosdns/rules}"
LKG_DIR="${LKG_DIR:-$STATE_DIR/lkg}"

age_of() {
  [ -f "$1" ] || { echo "-"; return; }
  m=$(find "$1" -maxdepth 0 -mmin -60 2>/dev/null | wc -l)
  if [ "$m" = 1 ]; then
    echo "$(find "$1" -maxdepth 0 -mmin -60 -printf '%s' 2>/dev/null | head -c0)$(find "$1" -maxdepth 0 -mmin -60 >/dev/null 2>&1 && echo '<60m')"
  else
    echo "$(find "$1" -maxdepth 0 -mmin -1440 >/dev/null 2>&1 && echo '>=1d' || echo '>=1h')"
  fi
}

cnt() { [ -f "$1" ] && grep -cE '^[^#[:space:]]' "$1" 2>/dev/null || echo 0; }

echo "== mosdns 运行状态"
if pgrep -f 'mosdns start' >/dev/null 2>&1; then
  pid=$(pgrep -f 'mosdns start' | head -1)
  echo "  running  pid=$pid  rss=$(awk '/VmRSS/{print $2" kB"}' /proc/$pid/status 2>/dev/null)"
else
  echo "  not running"
fi
echo
echo "== 规则源"
echo "  last_source: $(cat "$STATE_DIR/last_source" 2>/dev/null || echo '<未记录>')"
echo "  last_restart_ms: $(cat "$STATE_DIR/last_restart_ms" 2>/dev/null || echo '-') ms"
echo
echo "== 当前规则"
printf '  %-28s %8s\n' 文件 条数
for f in cn-domains.txt proxy-domains.txt gfw-domains.txt ai-domains.txt IPv4.China.list cn-exceptions.txt apple-cn.txt bogus-ip.txt custom-hosts.txt; do
  printf '  %-28s %8s\n' "$f" "$(cnt "$RULES/$f")"
done
echo
echo "== LKG"
if [ -d "$LKG_DIR" ]; then
  for f in cn-domains.txt proxy-domains.txt gfw-domains.txt IPv4.China.list; do
    printf '  %-28s %8s\n' "$f" "$(cnt "$LKG_DIR/$f")"
  done
else
  echo "  无 LKG 目录"
fi
echo
echo "== 上次同步日志（末尾 10 行）"
tail -n 10 "$STATE_DIR/sync.log" 2>/dev/null || echo "  无"
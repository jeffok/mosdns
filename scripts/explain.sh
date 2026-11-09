#!/bin/sh
# explain.sh <域名> [类型] — 解释一个域名会走哪条路径（规则匹配 + 实际查询）
#
# 输出：命中的名单类别、预期上游、实际应答、按 IP 归属的预期出口。
# 注意：这是「按规则文件的静态推断 + 一次真实查询」，不是引擎内部日志。
set -u
RULES="${RULES_DIR:-/etc/mosdns/rules}"
DNS="${EXPLAIN_DNS:-127.0.0.1}"
PORT="${DNS_ENTRY_PORT:-53}"

dom=${1:-}
qtype=${2:-A}
[ -n "$dom" ] || { echo "用法: explain.sh <域名> [A|AAAA|TXT]"; exit 2; }
id=$(echo "$dom" | tr 'A-Z' 'a-z' | sed 's/\.$//')
case "$PORT" in
  ''|*[!0-9]*) PORT=53 ;;
esac

# 域名是否命中某域名名单（后缀匹配，与 mosdns domain_set 语义一致）
hit() {
  f="$RULES/$1"
  [ -s "$f" ] || return 1
  awk -v d="$id" '
    /^[[:space:]]*#/ { next }
    {
      e=$1
      sub(/^(domain|full):/, "", e)
      if (d == e) { found=1; exit }
      n=length(e)
      if (length(d) > n && substr(d, length(d)-n) == "." e) { found=1; exit }
    }
    END { exit found ? 0 : 1 }' "$f"
}

class="unknown"
path="D3 未知域名策略"
if [ -s "$RULES/custom-hosts.txt" ] && awk -v d="$id" '
    /^[[:space:]]*#/ { next }
    /^[[:space:]]*$/ { next }
    {
      n=$1; typ="full"
      if (match(n, /^(domain|full):/)) { typ=substr(n,1,RLENGTH-1); n=substr(n,RLENGTH+1) }
      if (typ=="full" && n==d) { found=1; exit }
      if (typ=="domain" && (n==d || substr(d, length(d)-length(n)) == "." n)) { found=1; exit }
    }
    END { exit found ? 0 : 1 }' "$RULES/custom-hosts.txt"; then
  class="custom_hosts"; path="固定解析（hosts 直接应答，TTL 10s）"
elif hit cn-exceptions.txt; then
  class="cn_exceptions"; path="国内（D13 例外优先于硬名单）"
elif hit gfw-domains.txt; then
  class="gfw"; path="全球（硬名单，经隧道）"
elif hit ai-domains.txt; then
  class="ai"; path="全球（AI 硬名单，经隧道）"
elif hit proxy-domains.txt; then
  class="proxy"; path="软名单：D3 策略（默认 cn 优先 + 接受集判定）"
elif hit cn-domains.txt; then
  class="cn"; path="国内"
elif hit apple-cn.txt; then
  class="apple-cn"; path="国内"
fi

echo "域名: $id  类型: $qtype"
echo "命中: $class"
echo "路径: $path"
if [ "$qtype" = "AAAA" ] && [ "${ENABLE_IPV6:-0}" != 1 ]; then
  echo "预期: 空应答（D5，AAAA 统一返回空）"
fi

echo "实际应答:"
out=""
queried=1
if command -v dig >/dev/null 2>&1; then
  out=$(dig @"$DNS" -p "$PORT" +time=3 +tries=1 +short "$id" "$qtype" 2>/dev/null || true)
elif [ "$PORT" = 53 ]; then
  out=$(nslookup -type="$qtype" -timeout=3 "$id" "$DNS" 2>/dev/null | awk '/^Name:/{f=1;next} f && /^Address/{print $2}')
else
  echo "  （跳过查询：DNS_ENTRY_PORT=$PORT，当前环境没有 dig；busybox nslookup 不支持指定端口。请安装 bind-tools 或把 DNS_ENTRY_PORT 设为 53。）"
  queried=0
fi
if [ "$queried" = 1 ]; then
  [ -n "$out" ] || out="（无应答）"
  echo "$out" | sed 's/^/  /'
else
  out="（无应答）"
fi

# 对 A 记录做归属判断（CN / 私网 / 其它 → 预期出口）
if [ "$qtype" = "A" ]; then
  for ip in $out; do
    case "$ip" in ""|"（无应答）") continue ;; esac
    case "$ip" in *:*) continue ;; esac
    if awk -v ip="$ip" 'function toint(s,  a){split(s,a,"."); return a[1]*16777216+a[2]*65536+a[3]*256+a[4]}
        BEGIN{ipi=toint(ip)}
        /^[[:space:]]*#/ {next} /^[[:space:]]*$/ {next}
        { split($1,b,"/"); base=toint(b[1]); size=2^(32-b[2]); st=base-(base%size); if (ipi>=st && ipi<st+size) {f=1; exit} }
        END{exit f?0:1}' "$RULES/IPv4.China.list" 2>/dev/null; then
      echo "  $ip → 国内 IP → 预期出口 direct"
    else
      case "$ip" in
        10.*|192.168.*|172.1[6-9].*|172.2[0-9].*|172.3[01].*|127.*) echo "  $ip → 私网 → 预期出口 direct" ;;
        *) echo "  $ip → 非国内 → 预期出口 tunnel" ;;
      esac
    fi
  done
fi
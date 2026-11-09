#!/bin/sh
# validate.sh — 校验即将生效的规则，防止「静默退化」（方案 §14.3 / §19.2 / D7）
#
# 用法：
#   validate.sh                      校验 $STAGING（默认 $STATE_DIR/staging）
#   validate.sh --dir DIR            校验指定目录
#   validate.sh --custom-hosts FILE  只校验 custom-hosts.txt
#
# 校验内容：
#   1. 必需文件的绝对下限 + 相对上一版（LKG）不低于 80%
#   2. 每行格式（域名：单标签白名单；CIDR：掩码 0-32）
#   3. 哨兵断言（关键域名/网段必须落在预期名单里）
#   4. custom-hosts.txt：第一列必须是域名表达式（拦截「IP 域名」写反）、至少一个合法 IP、
#      默认拒绝 keyword:/regexp:、重复条目告警
#
# 退出码：0 通过；1 失败（调用方必须保持 LKG）；2 参数错误
set -u

STATE_DIR="${STATE_DIR:-/data/state}"
STAGING="${STAGING:-$STATE_DIR/staging}"
LKG_DIR="${LKG_DIR:-$STATE_DIR/lkg}"
MODE="rules"
TARGET=""

while [ $# -gt 0 ]; do
  case "$1" in
    --dir) TARGET=$2; shift 2 ;;
    --custom-hosts) MODE="custom"; TARGET=$2; shift 2 ;;
    -h|--help) sed -n '2,16p' "$0"; exit 0 ;;
    *) echo "未知参数 $1" >&2; exit 2 ;;
  esac
done

[ "$MODE" = "rules" ] && TARGET="${TARGET:-$STAGING}"

# 绝对下限（§14.3）；允许用环境变量覆盖以便测试
MIN_CN="${MIN_CN:-80000}"
MIN_PROXY="${MIN_PROXY:-15000}"
MIN_GFW="${MIN_GFW:-3000}"
MIN_AI="${MIN_AI:-20}"
MIN_IP="${MIN_IP:-4000}"
MIN_BOGUS="${MIN_BOGUS:-100}"
REL_FLOOR_PCT="${REL_FLOOR_PCT:-80}"

TMPD="${TMPDIR:-/tmp}/mosdns-validate.$$"
mkdir -p "$TMPD"
trap 'rm -rf "$TMPD"' EXIT INT TERM
ERRF="$TMPD/err"
BOGUS_TMP="$TMPD/bogus-ip"
WARNF="$TMPD/warn"
rm -f "$ERRF" "$WARNF"
err_count() { [ -f "$ERRF" ] && grep -c x "$ERRF" || printf 0; }
warn_count() { [ -f "$WARNF" ] && grep -c x "$WARNF" || printf 0; }
err() { echo "  ERROR: $*" >&2; echo x >> "$ERRF"; }
warn() { echo "  WARN: $*" >&2; echo x >> "$WARNF"; }
info() { echo "  $*"; }

count_lines() { grep -cE '^[^#[:space:]]' "$1" 2>/dev/null || printf 0; }

# 单标签白名单（品牌 TLD，§14.2-6）：不作为错误
SINGLE_LABEL_OK="baidu alibaba apple aws google cloud microsoft"

check_min() {
  f=$1; min=$2
  n=$(count_lines "$TARGET/$f")
  if [ "$n" -lt "$min" ]; then
    err "$f 条目数 $n < 绝对下限 $min"
  else
  info "$f: $n entries (min $min)"
  fi
  if [ -f "$LKG_DIR/$f" ]; then
    prev=$(count_lines "$LKG_DIR/$f")
    if [ "$prev" -gt 0 ]; then
      floor=$(( prev * REL_FLOOR_PCT / 100 ))
      if [ "$n" -lt "$floor" ]; then
        err "$f 条目数 $n < 上一版的 ${REL_FLOOR_PCT}%（$prev → 下限 $floor）"
      fi
    fi
  fi
}

# 域名格式：小写、无前缀、无空格、至少一个点（或白名单单标签）
check_domains() {
  f="$1"
  bad=$(awk -v ok="$SINGLE_LABEL_OK" '
    /^[[:space:]]*#/ { next }
    /^[[:space:]]*$/ { next }
    {
      d=$1
      if ($0 !~ /^[^$]+/) { print NR ": 含非法字符"; bad=1; next }
      if (d ~ /^[A-Z]/) { print NR ": 含大写"; bad=1; next }
      if (d ~ /^(domain|full|keyword|regexp):/) { print NR ": 含前缀"; bad=1; next }
      if (d ~ /^[*]/) { print NR ": 通配符"; bad=1; next }
      if (d !~ /[.]/) {
        n=split(ok, a, " ")
        hit=0
        for (i=1;i<=n;i++) if (d==a[i]) hit=1
        if (!hit) { print NR ": 单标签 " d }
      }
    }
    END { exit bad ? 1 : 0 }' "$TARGET/$f" 2>&1)
  if [ -n "$bad" ]; then
    echo "$bad" | head -10 | while read -r l; do
      case "$l" in
        *単标签*|*单标签*) warn "$f $l（品牌 TLD，按告警处理）" ;;
        *) err "$f $l" ;;
      esac
    done
    if echo "$bad" | grep -qv '单标签'; then return 1; fi
    return 0
  fi
  return 0
}

# CIDR 格式
check_cidr() {
  f="$1"
  bad=$(awk '
    /^[[:space:]]*#/ { next }
    /^[[:space:]]*$/ { next }
    {
      if (NF != 1) { print NR ": 多列"; bad=1; next }
      n=split($1, a, "/")
      if (n != 2) { print NR ": 缺掩码"; bad=1; next }
      o=split(a[1], s, ".")
      if (o != 4) { print NR ": 非 IPv4"; bad=1; next }
      for (i=1;i<=4;i++) if (s[i] !~ /^[0-9]+$/ || s[i] + 0 > 255) { print NR ": 非法八位组"; bad=1; next }
      if (a[2] !~ /^[0-9]+$/ || a[2] + 0 > 32) { print NR ": 掩码越界"; bad=1 }
    }
    END { exit bad ? 1 : 0 }' "$TARGET/$f" 2>&1)
  if [ -n "$bad" ]; then
    echo "$bad" | head -5 | while read -r l; do err "$f $l"; done
    return 1
  fi
  return 0
}

# 某域名是否落在某域名文件里（含子域，按后缀匹配）
domain_in() {
  d=$1; f=$2
  [ -s "$f" ] || return 1
  awk -v d="$d" '
    /^[[:space:]]*#/ { next }
    {
      e=$1
      sub(/^(domain|full):/, "", e)
      if (d == e) { found=1; exit }
      n=length(e)
      if (length(d) > n && substr(d, length(d) - n) == "." e) { found=1; exit }
    }
    END { exit found ? 0 : 1 }' "$f"
}

# 某 IP 是否在 CIDR 文件里
ip_in() {
  ip=$1; f=$2
  [ -s "$f" ] || return 1
  awk -v ip="$ip" '
    function toint(s,   a){split(s,a,"."); return a[1]*16777216 + a[2]*65536 + a[3]*256 + a[4]}
    BEGIN { ipi = toint(ip) }
    /^[[:space:]]*#/ { next }
    /^[[:space:]]*$/ { next }
    {
      split($1, b, "/"); base = toint(b[1]); mask = b[2] + 0
      size = 2 ^ (32 - mask)
      start = base - (base % size)
      if (ipi >= start && ipi < start + size) { found = 1; exit }
    }
    END { exit found ? 0 : 1 }' "$f"
}

check_sentinels() {
  # IP 哨兵
  for ip in 119.29.29.29 223.5.5.5 114.114.114.114 183.56.180.152 113.80.87.62; do
    ip_in "$ip" "$TARGET/IPv4.China.list" || err "哨兵 IP $ip 不在国内名单"
  done
  for ip in 8.8.8.8 1.1.1.1 9.9.9.9; do
    if ip_in "$ip" "$TARGET/IPv4.China.list"; then err "哨兵 IP $ip 不应在国内名单"; fi
  done
  # 私网/CGNAT 不应在 CN 名单（由接受集单独处理）
  for ip in 10.0.0.1 172.16.8.1 192.168.88.1 100.64.0.1; do
    if ip_in "$ip" "$TARGET/IPv4.China.list"; then warn "私网地址 $ip 出现在国内名单"; fi
  done
  # 域名哨兵
  domain_in baidu.com "$TARGET/cn-domains.txt" || err "哨兵域名 baidu.com 不在 cn-domains"
  if ! domain_in google.com "$TARGET/gfw-domains.txt" && ! domain_in google.com "$TARGET/proxy-domains.txt"; then
    err "哨兵域名 google.com 既不在 gfw 也不在 proxy"
  fi
  for d in chatgpt.com claude.ai; do
    if ! domain_in "$d" "$TARGET/ai-domains.txt" && ! domain_in "$d" "$TARGET/gfw-domains.txt"; then
      warn "AI 哨兵域名 $d 不在 ai/gfw 名单"
    fi
  done
  info "哨兵断言完成"
}

validate_rules() {
  [ -d "$TARGET" ] || { echo "目录不存在：$TARGET" >&2; exit 2; }
  echo "校验规则目录：$TARGET"
  for f in cn-domains.txt IPv4.China.list gfw-domains.txt proxy-domains.txt apple-cn.txt ai-domains.txt bogus-nxdomain.china.conf; do
    [ -f "$TARGET/$f" ] || { is_required "$f" && err "缺少必需文件 $f" || warn "缺少可选文件 $f"; }
  done
  [ -f "$TARGET/cn-domains.txt" ] && { check_min cn-domains.txt "$MIN_CN"; check_domains cn-domains.txt; }
  [ -f "$TARGET/proxy-domains.txt" ] && { check_min proxy-domains.txt "$MIN_PROXY"; check_domains proxy-domains.txt; }
  [ -f "$TARGET/gfw-domains.txt" ] && { check_min gfw-domains.txt "$MIN_GFW"; check_domains gfw-domains.txt; }
  [ -f "$TARGET/ai-domains.txt" ] && { check_min ai-domains.txt "$MIN_AI"; check_domains ai-domains.txt; }
  [ -f "$TARGET/IPv4.China.list" ] && { check_min IPv4.China.list "$MIN_IP"; check_cidr IPv4.China.list; }
  # 可选文件：缺失或为空只告警（线上尚未发布），出现时按格式校验
  for f in apple-cn.txt cn-exceptions.txt; do
    if [ -s "$TARGET/$f" ]; then
      check_domains "$f"
    else
      warn "可选文件 $f 为空或缺失（按空集合运行）"
    fi
  done
  # bogus：转成纯 IP 再校验
  if [ -s "$TARGET/bogus-nxdomain.china.conf" ]; then
    grep -oE '^bogus-nxdomain=[0-9]+[.][0-9]+[.][0-9]+[.][0-9]+$' "$TARGET/bogus-nxdomain.china.conf" \
      | sed 's/^bogus-nxdomain=//' | sort -u > "$BOGUS_TMP"
    n=$(grep -cE "^[0-9]" "$BOGUS_TMP" || printf 0)
    if [ "$n" -lt "$MIN_BOGUS" ]; then
      err "bogus IP 数量 $n < 下限 $MIN_BOGUS"
    else
      info "bogus IP: $n 条"
    fi
    rm -f "$BOGUS_TMP"
  else
    warn "bogus-nxdomain.china.conf 缺失（不做污染过滤）"
  fi
  check_sentinels
}

is_required() {
  case "$1" in
    cn-domains.txt|IPv4.China.list|gfw-domains.txt|proxy-domains.txt) return 0 ;;
    *) return 1 ;;
  esac
}

# custom-hosts.txt 逐行校验（§19.5）
validate_custom_hosts() {
  f="$1"
  [ -f "$f" ] || { echo "文件不存在：$f" >&2; exit 2; }
  echo "校验 custom-hosts：$f"
  out=$(awk '
    BEGIN { allow_kw = (ENVIRON["ALLOW_KEYWORD"] == "1") }
    /^[[:space:]]*#/ { next }
    /^[[:space:]]*$/ { next }
    {
      line = $0
      sub(/#.*/, "", line)
      gsub(/^[ \t]+|[ \t]+$/, "", line)
      if (line == "") next
      n = split(line, f, /[ \t]+/)
      if (n < 2) { printf "%d: 缺少 IP（格式应为「域名 IP」）\n", NR; bad=1; next }
      name = f[1]
      typ = "full"
      if (match(name, /^(domain|full|keyword|regexp):/)) {
        typ = substr(name, 1, RLENGTH - 1)
        name = substr(name, RLENGTH + 1)
      }
      if (typ == "keyword" || typ == "regexp") {
        if (!allow_kw) { printf "%d: 默认禁止 %s: 表达式\n", NR, typ; bad=1; next }
      }
      if (name ~ /^[0-9]+[.][0-9]+[.][0-9]+[.][0-9]+$/) {
        printf "%d: 第一列是 IP，格式写反了（应为「域名 IP」）\n", NR; bad=1; next
      }
      if (name == "") { printf "%d: 缺少域名\n", NR; bad=1; next }
      if (seen[name]++) printf "%d: 重复条目 %s\n", NR, name
      for (i = 2; i <= n; i++) {
        ip = f[i]
        if (ip ~ /^[0-9]+[.][0-9]+[.][0-9]+[.][0-9]+$/) {
          split(ip, o, ".")
          for (k = 1; k <= 4; k++) if (o[k] + 0 > 255) { printf "%d: 非法 IPv4 %s\n", NR, ip; bad = 1 }
        } else if (ip ~ /^[0-9A-Fa-f:]+$/) {
          # 粗略校验 IPv6：至少两个冒号
          c = gsub(/:/, ":", ip)
          if (c < 2) { printf "%d: 非法 IPv6 %s\n", NR, ip; bad = 1 }
        } else {
          printf "%d: 非法 IP %s\n", NR, ip; bad = 1
        }
      }
    }
    END { exit bad ? 1 : 0 }' "$f" 2>&1)
  if [ -n "$out" ]; then
    echo "$out" | head -20 | while read -r l; do echo "  ERROR: $l" >&2; echo x >> "$ERRF"; done
    echo "custom-hosts 校验失败" >&2
    return 1
  fi
  cnt=$(count_lines "$f")
  info "custom-hosts: $cnt 条有效条目"
  return 0
}

case "$MODE" in
  custom) validate_custom_hosts "$TARGET"; rc=$? ;;
  rules)  validate_rules; rc=0 ;;
esac

# 注意：Errors 在子 shell 中累加不会回传，因此以 awk 输出与返回值判定
if [ "$MODE" = "custom" ]; then
  [ "$rc" = 0 ] || exit 1
fi

E=$(err_count); W=$(warn_count)
if [ "$E" -gt 0 ]; then
  echo "校验失败：$E 个错误、$W 个告警" >&2
  exit 1
fi
echo "校验通过（告警 $W 个）"
exit 0

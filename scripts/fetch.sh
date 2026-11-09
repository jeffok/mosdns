#!/bin/sh
# fetch.sh — 从有序规则源抓取 ASN-China release-files 到 staging 目录
#
# 规则（方案 §19.4 / D20）：
#   * RULES_SOURCES 是逗号分隔的有序 base URL 列表；每个 URL + "/文件名" 即可下载
#   * 上次成功的源优先（粘性，状态存 $STATE_DIR/last_source）
#   * 同一次更新的所有文件只取自同一个源，不混用
#   * 必需文件缺失/为空 → 该源作废，换下一个；可选文件缺失只告警
#   * 全部源失败 → 退出码 2，保持 LKG 不动
#
# 用法：
#   fetch.sh                 抓取到 $STAGING
#   fetch.sh --probe         只探测、不下载，输出各源可达性/耗时/文件齐全（Markdown 表）
#
set -u

STATE_DIR="${STATE_DIR:-/data/state}"
STAGING="${STAGING:-$STATE_DIR/staging}"
PROBE_TIMEOUT="${RULES_PROBE_TIMEOUT:-5}"
FETCH_TIMEOUT="${RULES_FETCH_TIMEOUT:-60}"
DEFAULT_SOURCES="https://gh-proxy.com/https://raw.githubusercontent.com/jeffok/ASN-China/release-files,https://raw.githubusercontent.com/jeffok/ASN-China/release-files,https://gitea.jeffok.com:4443/jeff/asn-china/raw/branch/release-files"
RULES_SOURCES="${RULES_SOURCES-$DEFAULT_SOURCES}"
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)

# 必需文件（缺失即该源作废）
REQUIRED_FILES="cn-domains.txt IPv4.China.list gfw-domains.txt proxy-domains.txt ai-domains.txt"
# 可选文件（线上可能还没发布：cn-exceptions.txt 尚不存在、apple-cn.txt 为空）
OPTIONAL_FILES="apple-cn.txt cn-exceptions.txt bogus-nxdomain.china.conf"
ALL_FILES="$REQUIRED_FILES $OPTIONAL_FILES"
PROBE_FILE="gfw-domains.txt"

log() {
  line="[$(date '+%H:%M:%S')] [fetch] $*"
  printf '%s\n' "$line"
  printf '%s\n' "$line" >> "$STATE_DIR/fetch.log"
}
warn() {
  line="[$(date '+%H:%M:%S')] [fetch] WARN $*"
  printf '%s\n' "$line" >&2
  printf '%s\n' "$line" >> "$STATE_DIR/fetch.log"
}

strip_secret() { printf '%s' "$1" | sed -E 's#(://)[^/@]+@#\1***@#'; }

# busybox wget：$1 超时 $2 输出 $3 URL
wget_common() {
  wget -q -T "$1" -O "$2" "$3" 2>/dev/null
}

is_optional() {
  case " $OPTIONAL_FILES " in
    *" $1 "*) return 0 ;;
    *) return 1 ;;
  esac
}

# 计时（秒，两位小数）
now_ms() {
  awk '{ printf "%.0f", $1 * 1000 }' /proc/uptime 2>/dev/null \
    || echo $(( $(date +%s) * 1000 ))
}

write_resolv_for_fetch() {
  # FETCH_RESOLVE="gitea.jeffok.com:4443:192.168.88.252"：让同步不依赖 DNS（写入 /etc/hosts）
  [ -n "${FETCH_RESOLVE:-}" ] || return 0
  _host=${FETCH_RESOLVE%%:*}; _rest=${FETCH_RESOLVE#*:}; _ip=${_rest##*:}
  [ -n "$_host" ] && [ -n "$_ip" ] || return 0
  if ! grep -qE "^[0-9.]+[[:space:]]+$_host([[:space:]]|\$)" /etc/hosts 2>/dev/null; then
    printf '%s %s # FETCH_RESOLVE\n' "$_ip" "$_host" >> /etc/hosts
    log "FETCH_RESOLVE: $_host -> $_ip"
  fi
}

ordered_sources() {
  sticky=""
  [ -f "$STATE_DIR/last_source" ] && sticky=$(cat "$STATE_DIR/last_source")
  # A removed source must never survive a configuration change via sticky state.
  configured=$(printf '%s' "$RULES_SOURCES" | tr -d '[:space:]')
  case ",$configured," in
    *,"$sticky",*) ;;
    *) sticky= ;;
  esac

  if [ -n "$sticky" ]; then printf '%s\n' "$sticky"; fi
  echo "$RULES_SOURCES" | tr ',' '\n' | while read -r s; do
    s=$(echo "$s" | tr -d ' ')
    [ -n "$s" ] || continue
    [ "$s" = "$sticky" ] && continue
    printf '%s\n' "$s"
  done
}

# 探测一个源：可达性 + 耗时（毫秒）
probe_source() {
  base="${1%/}"
  t0=$(now_ms)
  if wget_common "$PROBE_TIMEOUT" /dev/null "$base/$PROBE_FILE"; then
    t1=$(now_ms)
    echo $(( t1 - t0 ))
    return 0
  fi
  echo "-"
  return 1
}

# 只下载文件头，用于判断文件是否齐全（HEAD 更省，但 busybox wget 无 --spider 元数据）
list_ok() {
  base="${1%/}"
  missing=""
  for f in $ALL_FILES; do
    out=/dev/null
    if wget_common "$PROBE_TIMEOUT" "$out" "$base/$f"; then
      is_optional "$f" || continue
    else
      if is_optional "$f"; then
        missing="$missing $f(optional)"
      else
        missing="$missing $f(MISSING)"
      fi
    fi
  done
  printf '%s' "$missing"
}

do_probe() {
  echo "| 源 | 可达 | 耗时 | 缺失文件 | 与 LKG | 结论 |"
  echo "|---|---|---|---|---|---|"
  sticky=""
  [ -f "$STATE_DIR/last_source" ] && sticky=$(cat "$STATE_DIR/last_source")
  echo "$RULES_SOURCES" | tr ',' '\n' | while read -r s; do
    s=$(echo "$s" | tr -d ' ')
    [ -n "$s" ] || continue
    if ms=$(probe_source "$s"); then
      miss=$(list_ok "$s")
      [ -z "$miss" ] && miss="无"
      same=$(cmp -s "$STATE_DIR/lkg/gfw-domains.txt" "$STAGING/gfw-domains.txt" 2>/dev/null && echo 相同 || echo 待下载)
      if [ -n "$miss" ] && ! echo "$miss" | grep -q MISSING; then
        verdict="可用（可选文件缺失）"
      elif [ -n "$miss" ]; then
        verdict="拒绝（必需文件缺失）"
      else
        verdict="可用"
      fi
      tag=""
      [ "$s" = "$sticky" ] && tag="（上次成功）"
      echo "| $(strip_secret "$s")$tag | 是 | ${ms}ms | $miss | $same | $verdict |"
    else
      echo "| $(strip_secret "$s") | 否 | - | - | - | 跳过 |"
    fi
  done
}

fetch_all_from() {
  base="${1%/}"
  rm -rf "$STAGING"; mkdir -p "$STAGING"
  for f in $ALL_FILES; do
    if ! wget_common "$FETCH_TIMEOUT" "$STAGING/$f" "$base/$f"; then
      if is_optional "$f"; then
        : > "$STAGING/$f"
        warn "可选文件 $f 在 $base 不可用（忽略）"
        continue
      fi
      warn "必需文件 $f 下载失败，放弃该源"
      return 1
    fi
    if [ ! -s "$STAGING/$f" ] && ! is_optional "$f"; then
      warn "必需文件 $f 为空，放弃该源"
      return 1
    fi
  done
  return 0
}

validate_staging() {
  # Exact equality to the previously validated LKG needs no repeated validation.
  unchanged=1
  for file in $ALL_FILES; do
    if ! cmp -s "$STAGING/$file" "${LKG_DIR:-$STATE_DIR/lkg}/$file"; then unchanged=0; break; fi
  done
  if [ "$unchanged" = 1 ]; then
    log "规则与已验证 LKG 无变化，跳过校验"
    return 0
  fi
  validator="$SCRIPT_DIR/validate.sh"
  [ -x "$validator" ] || validator=/opt/mosdns/validate.sh
  [ -x "$validator" ] || { warn "validate.sh is unavailable"; return 1; }
  STATE_DIR="$STATE_DIR" LKG_DIR="${LKG_DIR:-$STATE_DIR/lkg}" "$validator" --dir "$STAGING"
}

main() {
  mkdir -p "$STATE_DIR"
  write_resolv_for_fetch
  if [ "${1:-}" = "--probe" ]; then do_probe; return 0; fi
  : > "$STATE_DIR/fetch.log"

  for base in $(ordered_sources); do
    log "尝试源 $(strip_secret "$base")"
    if probe_source "$base" >/dev/null && fetch_all_from "$base" && validate_staging; then
      echo "$base" > "$STATE_DIR/last_source"
      log "选用源 $(strip_secret "$base")"
      return 0
    fi
    warn "源不可用或校验失败：$(strip_secret "$base")"
  done
  warn "所有源都失败，保持 LKG"
  return 2
}

main "$@"

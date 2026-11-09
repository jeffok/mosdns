#!/bin/sh
# mosdns 容器入口：渲染配置 → 启动 mosdns（PID1 监督）→ 并列运行同步循环
#
# 设计要点（方案 §3.2 / D5 / D9 / D10 / D13 / D15 / D19 / §19）：
#   1. 先用「挂载的 rules/ 或镜像内烘焙的 rules/」启动 mosdns，保证可用性；
#   2. 可选文件（cn-exceptions.txt、apple-cn.txt）缺失或为空时只告警，生成同名空文件，
#      不让校验/配置加载失败 —— 线上 release-files 目前还没有前者、后者为空；
#   3. 同步循环与 mosdns 并列：循环崩溃不影响 DNS，mosdns 退出会被重新拉起；
#   4. 已有同步脚本套件（fetch/validate/apply）时由它接管更新，否则用内置的简易同步。
set -eu

RULES=${RULES_DIR:-/etc/mosdns/rules}
BAKED=/opt/mosdns-rules
STATE=${STATE_DIR:-/data/state}
CONFIG=${MOSDNS_CONFIG:-/etc/mosdns/config.yaml}
PIDFILE=/tmp/mosdns.pid
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)

log() { echo "[$(date '+%Y-%m-%dT%H:%M:%S%z')] [entrypoint] $*"; }
warn() { echo "[$(date '+%Y-%m-%dT%H:%M:%S%z')] [entrypoint] WARN $*" >&2; }

mkdir -p "$RULES" "$STATE" "$(dirname "$CONFIG")"

# ---------------------------------------------------------------- 容器内解析
# 默认 DNSPod 公共 DNS。jeffok.com 按线路解析，不同解析器结果不同属正常。
# 显式设为空字符串时同样回退默认值，不报错。
# szhome 访问内网 Gitea 请用 FETCH_RESOLVE=gitea.jeffok.com:4443:192.168.88.252。
CONTAINER_DNS="${CONTAINER_DNS:-119.29.29.29}"
CONTAINER_DNS=$(printf '%s' "$CONTAINER_DNS" | tr -d '[:space:]')
[ -n "$CONTAINER_DNS" ] || CONTAINER_DNS=119.29.29.29
echo "nameserver $CONTAINER_DNS" > /etc/resolv.conf
log "resolv.conf -> $CONTAINER_DNS"

# ---------------------------------------------------------------- 可选文件清单
# 线上 ASN-China release-files 尚未发布 cn-exceptions.txt，apple-cn.txt 为空（2026-11-09）。
# 这两个文件缺失/为空只告警，不阻断启动；下限阈值可配，默认 0（关闭）。
CN_EXCEPTIONS_MIN=${CN_EXCEPTIONS_MIN:-0}
APPLE_CN_MIN=${APPLE_CN_MIN:-0}

# 必须存在的规则文件（缺失且镜像内也没有时用空文件兜底并告警，由同步循环补齐）
REQUIRED_FILES="cn-domains.txt IPv4.China.list gfw-domains.txt ai-domains.txt proxy-domains.txt"

is_optional() {
  case "$1" in
    cn-exceptions.txt|apple-cn.txt|ai-domains.txt|bogus-ip.txt|accept-extra.txt) return 0 ;;
    *) return 1 ;;
  esac
}

# 从镜像烘焙目录补齐缺失文件（不覆盖已有文件）
init_rules() {
  [ -d "$BAKED" ] || { warn "no baked rules dir $BAKED"; return 0; }
  for f in "$BAKED"/*; do
    [ -f "$f" ] || continue
    name=$(basename "$f")
    if [ ! -f "$RULES/$name" ]; then
      cp "$f" "$RULES/$name" && log "initialized $name from image"
    fi
  done
}

# 可选文件预检：缺失或空 → 生成空文件 + 告警（mosdns 需要文件存在才能加载）
prepare_optional_files() {
  for name in cn-exceptions.txt apple-cn.txt; do
    if [ ! -f "$RULES/$name" ]; then
      : > "$RULES/$name"
      warn "optional rule $name is missing, using empty set (no failure)"
      continue
    fi
    n=$(grep -cE '^[^#[:space:]]' "$RULES/$name" 2>/dev/null || true)
    n=${n:-0}
    if [ "$n" -eq 0 ]; then
      warn "optional rule $name is empty, using empty set (no failure)"
    fi
  done
  for spec in "cn-exceptions.txt:$CN_EXCEPTIONS_MIN" "apple-cn.txt:$APPLE_CN_MIN"; do
    name=${spec%%:*}; min=${spec##*:}
    [ "$min" -gt 0 ] 2>/dev/null || continue
    n=$(grep -cE '^[^#[:space:]]' "$RULES/$name" 2>/dev/null || printf 0)
    if [ "$n" -lt "$min" ]; then
      warn "optional rule $name has $n entries < CN threshold $min (not fatal by default)"
    fi
  done
}

# 接受集附加段（D3）：RFC1918 + 100.64/10
write_accept_extra() {
  cat > "$RULES/accept-extra.txt" <<'EOFEXTRA'
# 接受集附加段：私网与 CGNAT（CN 名单不包含这些段）
10.0.0.0/8
172.16.0.0/12
192.168.0.0/16
100.64.0.0/10
EOFEXTRA
}

# bogus 清单转换（dnsmasq → 纯 IP 列表），D10
convert_bogus() {
  src="$RULES/bogus-nxdomain.china.conf"
  [ -f "$src" ] || { : > "$RULES/bogus-ip.txt"; return 0; }
  {
    echo "# 由 bogus-nxdomain.china.conf 转换（每行一个 IP）"
    grep -oE '^bogus-nxdomain=[0-9]+[.][0-9]+[.][0-9]+[.][0-9]+$' "$src" | sed 's/^bogus-nxdomain=//' | sort -u
  } > "$RULES/bogus-ip.txt"
  log "bogus-ip.txt: $(grep -cE '^[0-9]' "$RULES/bogus-ip.txt") entries"
}

# ---------------------------------------------------------------- 配置渲染
# 模板用 ${VAR} 形式引用环境变量，sed 只替换已知占位符（避免误伤 $tag）
yaml_escape() { printf '%s' "$1" | sed -e 's/[\\\"]/\\\\&/g'; }

dns_to_yaml() {
  echo "$1" | tr ',' '\n' | while read -r addr; do
    addr=$(echo "$addr" | tr -d ' ')
    [ -n "$addr" ] && printf '    - addr: %s\n' "$addr"
  done
}

count_dns() {
  n=$(echo "$1" | tr ',' '\n' | grep -c '[^[:space:]]' || true)
  [ "${n:-0}" -lt 1 ] && n=1
  echo "$n"
}

render_config() {
  [ -f "$SCRIPT_DIR/config.template.yaml" ] && TEMPLATE="$SCRIPT_DIR/config.template.yaml" || TEMPLATE=/opt/mosdns/config.template.yaml
  [ -f "$TEMPLATE" ] || { warn "config template not found: $TEMPLATE"; return 1; }

  DNS_CN="${DNS_CN:-119.29.29.29,223.5.5.5,114.114.114.114}"
  # D4/Q25：szhome 目前只有 8.8.8.8 有隧道路由，默认只用它；加 1.1.1.1 需先补隧道路由
  DNS_GLOBAL="${DNS_GLOBAL:-8.8.8.8}"
  DNS_AI="${DNS_AI:-$DNS_GLOBAL}"
  ECS_PRESET="${ECS_PRESET:-}"
  CONCURRENT_CN="${CONCURRENT_CN:-$(count_dns "$DNS_CN")}"
  CONCURRENT_GLOBAL="${CONCURRENT_GLOBAL:-$(count_dns "$DNS_GLOBAL")}"
  CONCURRENT_AI="${CONCURRENT_AI:-$(count_dns "$DNS_AI")}"

  case "${UNKNOWN_POLICY:-cn_first}" in
    cn_first)     FALLBACK_PRIMARY=query_cn_first;    FALLBACK_SECONDARY=global_sequence ;;
    global_first) FALLBACK_PRIMARY=query_global_first; FALLBACK_SECONDARY=cn_sequence ;;
    *) warn "UNKNOWN_POLICY=$UNKNOWN_POLICY 无效，按 cn_first 处理"
       FALLBACK_PRIMARY=query_cn_first; FALLBACK_SECONDARY=global_sequence ;;
  esac
  FALLBACK_THRESHOLD="${FALLBACK_THRESHOLD:-300}"
  FALLBACK_STANDBY="${FALLBACK_STANDBY:-true}"
  DNS_ENTRY_LISTEN="${DNS_ENTRY_LISTEN:-0.0.0.0:53}"
  DNS_API_LISTEN="${DNS_API_LISTEN:-127.0.0.1:9091}"
  MOSDNS_LOG_LEVEL="${MOSDNS_LOG_LEVEL:-info}"
  MOSDNS_LOG_FILE="${MOSDNS_LOG_FILE:-/etc/mosdns/mosdns.log}"
  CACHE_SIZE="${CACHE_SIZE:-10240}"
  LAZY_CACHE_TTL="${LAZY_CACHE_TTL:-120}"
  CACHE_DUMP_INTERVAL="${CACHE_DUMP_INTERVAL:-1800}"
  CACHE_DUMP_FILE="${CACHE_DUMP_FILE:-/data/cache.dump}"

  UPSTREAMS_CN=$(dns_to_yaml "$DNS_CN")
  UPSTREAMS_GLOBAL=$(dns_to_yaml "$DNS_GLOBAL")
  UPSTREAMS_AI=$(dns_to_yaml "$DNS_AI")

  DOH_PLUGIN=""
  case "${DOH_ENABLED:-0}" in
    1|true|yes)
      DOH_CERT="${DOH_CERT:-/etc/mosdns/certs/fullchain.pem}"
      DOH_KEY="${DOH_KEY:-/etc/mosdns/certs/privkey.pem}"
      DOH_LISTEN="${DOH_LISTEN:-0.0.0.0:8443}"
      if [ -f "$DOH_CERT" ] && [ -f "$DOH_KEY" ]; then
        DOH_PLUGIN=$(printf '%s\n' \
          '- tag: doh_server' \
          '  type: http_server' \
          '  args:' \
          '    entries:' \
          '    - path: /dns-query' \
          '      exec: main_sequence' \
          "    listen: $DOH_LISTEN" \
          "    cert: $DOH_CERT" \
          "    key: $DOH_KEY" \
          '    idle_timeout: 10')
        log "DoH enabled on $DOH_LISTEN"
      else
        DOH_CERT=""; DOH_KEY=""
        warn "DOH_ENABLED=1 但缺少证书/私钥（$DOH_CERT, $DOH_KEY），已跳过 DoH"
      fi ;;
  esac

  # 多行块与长值先落到临时文件，再用 awk 按 %标记% 逐行替换（busybox awk 无 -v 多行支持）
  blockdir="$STATE/.render"
  rm -rf "$blockdir"; mkdir -p "$blockdir"
  printf '%s\n' "$UPSTREAMS_CN" > "$blockdir/UPSTREAMS_CN"
  printf '%s\n' "$UPSTREAMS_GLOBAL" > "$blockdir/UPSTREAMS_GLOBAL"
  printf '%s\n' "$UPSTREAMS_AI" > "$blockdir/UPSTREAMS_AI"
  : > "$blockdir/DOH_PLUGIN"
  [ -n "$DOH_PLUGIN" ] && printf '%s\n' "$DOH_PLUGIN" > "$blockdir/DOH_PLUGIN"

  # 标量值（含引号转义）：整体替换 %NAME%
  : > "$blockdir/scalars"
  set_scalar() {
    printf '%s\t%s\n' "$1" "$2" >> "$blockdir/scalars"
  }
  set_scalar CONCURRENT_CN "$CONCURRENT_CN"
  set_scalar CONCURRENT_GLOBAL "$CONCURRENT_GLOBAL"
  set_scalar CONCURRENT_AI "$CONCURRENT_AI"
  set_scalar ECS_PRESET "$(yaml_escape "$ECS_PRESET")"
  set_scalar FALLBACK_PRIMARY "$FALLBACK_PRIMARY"
  set_scalar FALLBACK_SECONDARY "$FALLBACK_SECONDARY"
  set_scalar FALLBACK_THRESHOLD "$FALLBACK_THRESHOLD"
  set_scalar FALLBACK_STANDBY "$FALLBACK_STANDBY"
  set_scalar DNS_ENTRY_LISTEN "$DNS_ENTRY_LISTEN"
  set_scalar DNS_API_LISTEN "$DNS_API_LISTEN"
  set_scalar MOSDNS_LOG_LEVEL "$MOSDNS_LOG_LEVEL"
  set_scalar MOSDNS_LOG_FILE "$MOSDNS_LOG_FILE"
  set_scalar CACHE_SIZE "$CACHE_SIZE"
  set_scalar LAZY_CACHE_TTL "$LAZY_CACHE_TTL"
  set_scalar CACHE_DUMP_INTERVAL "$CACHE_DUMP_INTERVAL"
  set_scalar CACHE_DUMP_FILE "$CACHE_DUMP_FILE"
  set_scalar DOH_LISTEN "${DOH_LISTEN:-}"
  set_scalar DOH_CERT "${DOH_CERT:-}"
  set_scalar DOH_KEY "${DOH_KEY:-}"

  awk -v dir="$blockdir" '
    function block(name,   l) {
      while ((getline l < (dir "/" name)) > 0) print l
      close(dir "/" name)
    }
    BEGIN {
      while ((getline sv < (dir "/scalars")) > 0) {
        i = index(sv, "\t")
        n = substr(sv, 1, i - 1)
        v[n] = substr(sv, i + 1)
      }
      close(dir "/scalars")
    }
    /^%UPSTREAMS_CN%$/      { block("UPSTREAMS_CN"); next }
    /^%UPSTREAMS_GLOBAL%$/  { block("UPSTREAMS_GLOBAL"); next }
    /^%UPSTREAMS_AI%$/      { block("UPSTREAMS_AI"); next }
    /^%DOH_PLUGIN%$/        { block("DOH_PLUGIN"); next }
    {
      line = $0
      out = ""
      while (match(line, "%[A-Z_]+%")) {
        n = substr(line, RSTART + 1, RLENGTH - 2)
        out = out substr(line, 1, RSTART - 1) v[n]
        line = substr(line, RSTART + RLENGTH)
      }
      print out line
    }' "$TEMPLATE" > "$CONFIG.tmp.new" || { warn "render failed"; return 1; }

  RENDERED=$(cat "$CONFIG.tmp.new")
  rm -f "$CONFIG.tmp.new"

  if [ "${ENABLE_IPV6:-0}" = 1 ] || [ "${ENABLE_IPV6:-0}" = true ] || [ "${ENABLE_IPV6:-0}" = yes ]; then
    printf '%s\n' "$RENDERED" | grep -v -x -e '  - matches: qtype 28' -e '    exec: reject 0' > "$CONFIG.tmp.new" || true
    RENDERED=$(cat "$CONFIG.tmp.new")
    rm -f "$CONFIG.tmp.new"
    log "ENABLE_IPV6=1：已关闭 AAAA 空应答"
  fi

  printf '%s\n' "$RENDERED" > "$CONFIG.tmp" && mv "$CONFIG.tmp" "$CONFIG"
  rm -rf "$blockdir"
  log "config.yaml 已生成: CN=$(count_dns "$DNS_CN") GLOBAL=$(count_dns "$DNS_GLOBAL") AI=$(count_dns "$DNS_AI") policy=${UNKNOWN_POLICY:-cn_first} ecs=${ECS_PRESET:-<empty>} ipv6=${ENABLE_IPV6:-0}"
}

check_config_files() {
  missing=0
  for f in $(grep -oE '^[[:space:]]*- /etc/mosdns/rules/.+$' "$CONFIG" | sed -e 's|^[[:space:]]*- /etc/mosdns/rules/||'); do
    if [ ! -f "$RULES/$f" ]; then
      warn "config references missing rule file $f"
      missing=1
    fi
  done
  return $missing
}

# ---------------------------------------------------------------- mosdns 监督
MOSDNS_PID=""
MOSDNS_RESTARTS=0

start_mosdns() {
  log "starting mosdns"
  mosdns start -c "$CONFIG" &
  MOSDNS_PID=$!
  echo "$MOSDNS_PID" > "$PIDFILE"
}

stop_mosdns() {
  [ -n "$MOSDNS_PID" ] || return 0
  kill -TERM "$MOSDNS_PID" 2>/dev/null || true
  wait "$MOSDNS_PID" 2>/dev/null || true
  MOSDNS_PID=""
}

# 同步循环用的钩子：规则就位后，SIGTERM 主实例，由 supervise 拉起
restart_mosdns() {
  if [ -z "$MOSDNS_PID" ]; then
    warn "restart requested but mosdns is not running"
    return 1
  fi
  log "SIGTERM mosdns pid $MOSDNS_PID for rule reload"
  kill -TERM "$MOSDNS_PID" 2>/dev/null || true
  return 0
}

# ---------------------------------------------------------------- 同步辅助（内置兜底用）
RULES_SOURCES="${RULES_SOURCES-https://gh-proxy.com/https://raw.githubusercontent.com/jeffok/ASN-China/release-files,https://raw.githubusercontent.com/jeffok/ASN-China/release-files,https://gitea.jeffok.com:4443/jeff/asn-china/raw/branch/release-files}"
RULES_FILES="cn-domains.txt IPv4.China.list gfw-domains.txt proxy-domains.txt apple-cn.txt cn-exceptions.txt ai-domains.txt bogus-nxdomain.china.conf"
RULES_PROBE_FILE="gfw-domains.txt"
RULES_PROBE_TIMEOUT="${RULES_PROBE_TIMEOUT:-5}"
RULES_FETCH_TIMEOUT="${RULES_FETCH_TIMEOUT:-60}"
LAST_SOURCE_FILE="$STATE/last_source"
SYNC_LOG="$STATE/sync.log"

# 下载（busybox wget）：$1 超时 $2 输出 $3 URL [$4 额外 header]
fetch_url() {
  _t="$1"; _o="$2"; _u="$3"; _h="${4:-}"
  if [ -n "$_h" ]; then
    wget -q -T "$_t" --header "$_h" -O "$_o" "$_u" 2>>"$SYNC_LOG"
  else
    wget -q -T "$_t" -O "$_o" "$_u" 2>>"$SYNC_LOG"
  fi
}

write_resolv_for_fetch() {
  # FETCH_RESOLVE="gitea.jeffok.com:4443:192.168.88.252"：让同步不依赖 DNS 解析
  # 会写入 /etc/hosts（按域名取证书仍一致），仅作可选开关
  [ -n "${FETCH_RESOLVE:-}" ] || return 0
  _host=${FETCH_RESOLVE%%:*}; _rest=${FETCH_RESOLVE#*:}; _port=${_rest%%:*}; _ip=${_rest##*:}
  [ -n "$_host" ] && [ -n "$_ip" ] || return 0
  if ! grep -qE "^[0-9.]+[[:space:]]+$_host([[:space:]]|$$)" /etc/hosts 2>/dev/null; then
    printf '%s %s # FETCH_RESOLVE\n' "$_ip" "$_host" >> /etc/hosts
    log "FETCH_RESOLVE: $_host -> $_ip (port $_port)"
  fi
}

probe_source() {
  base="${1%/}"
  fetch_url "$RULES_PROBE_TIMEOUT" /dev/null "$base/$RULES_PROBE_FILE"
}

ordered_sources() {
  sticky=""
  [ -f "$LAST_SOURCE_FILE" ] && sticky=$(cat "$LAST_SOURCE_FILE")
  if [ -n "$sticky" ]; then printf '%s\n' "$sticky"; fi
  echo "$RULES_SOURCES" | tr ',' '\n' | while read -r s; do
    s=$(echo "$s" | tr -d ' ')
    [ -n "$s" ] || continue
    [ "$s" = "$sticky" ] && continue
    printf '%s\n' "$s"
  done
}

fetch_all_from() {
  base="${1%/}"
  stage="$STATE/staging"
  rm -rf "$stage"; mkdir -p "$stage"
  for f in $RULES_FILES; do
    if ! fetch_url "$RULES_FETCH_TIMEOUT" "$stage/$f" "$base/$f"; then
      if is_optional "$f"; then
        warn "optional $f not available from $base (ignored)"
        : > "$stage/$f"
        continue
      fi
      warn "fetch $f from $base failed"
      return 1
    fi
  done
  # 关键文件必须非空，否则视为该源失效（D7：不得产生空规则）
  for f in cn-domains.txt IPv4.China.list gfw-domains.txt proxy-domains.txt; do
    if [ ! -s "$stage/$f" ]; then
      warn "$base returned empty $f"
      return 1
    fi
  done
  return 0
}

install_staged() {
  stage="$STATE/staging"
  changed=0
  for f in $RULES_FILES; do
    [ -f "$stage/$f" ] || continue
    # 可选文件：源上取不到（空）时不覆盖现有内容
    if is_optional "$f" && [ ! -s "$stage/$f" ] && [ -s "$RULES/$f" ]; then
      continue
    fi
    if [ -f "$RULES/$f" ] && cmp -s "$stage/$f" "$RULES/$f"; then
      continue
    fi
    cp "$stage/$f" "$RULES/$f.new" && mv "$RULES/$f.new" "$RULES/$f"
    changed=1
  done
  [ "$changed" = 1 ] && convert_bogus
  log "install_staged changed=$changed"
}

sync_rules_once() {
  write_resolv_for_fetch
  for base in $(ordered_sources); do
    log "probe rules source $base"
    if probe_source "$base" && fetch_all_from "$base"; then
      echo "$base" > "$LAST_SOURCE_FILE"
      install_staged
      log "rules synced from $base"
      restart_mosdns || true
      return 0
    fi
    warn "rules source unusable: $base"
  done
  warn "all rules sources failed, keep last known good"
  return 1
}

# custom-hosts：单主源；主源不可达时保持当前文件，不切到其它源（§19.5 / GS-15）
CUSTOM_HOSTS_SOURCES="${CUSTOM_HOSTS_SOURCES-https://gitea.jeffok.com:4443/jeff/mosdns/raw/branch/main/rules/custom-hosts.txt}"
CUSTOM_HOSTS_INTERVAL="${CUSTOM_HOSTS_INTERVAL:-120}"

sync_custom_hosts_once() {
  [ -n "$CUSTOM_HOSTS_SOURCES" ] || return 0
  auth="${CUSTOM_HOSTS_AUTH:-}"
  hdr=""
  [ -n "$auth" ] && hdr="Authorization: $auth"
  for url in $(echo "$CUSTOM_HOSTS_SOURCES" | tr ',' '\n'); do
    url=$(echo "$url" | tr -d ' ')
    [ -n "$url" ] || continue
    tmp="$STATE/custom-hosts.new"
    if ! fetch_url "$RULES_PROBE_TIMEOUT" "$tmp" "$url" "$hdr" || [ ! -s "$tmp" ]; then
      warn "custom-hosts source unreachable, keep current file: $(strip_secret "$url")"
      rm -f "$tmp"
      return 1
    fi
    if [ -f "$RULES/custom-hosts.txt" ] && cmp -s "$tmp" "$RULES/custom-hosts.txt"; then
      rm -f "$tmp"
      return 0
    fi
    val="$SCRIPT_DIR/validate.sh"
    [ -x "$val" ] || val="/opt/mosdns/validate.sh"
    if [ -x "$val" ]; then
      if ! "$val" --custom-hosts "$tmp" >>"$SYNC_LOG" 2>&1; then
        warn "custom-hosts validation failed, keep current file"
        rm -f "$tmp"
        return 1
      fi
    elif ! valid_custom_hosts "$tmp"; then
      warn "custom-hosts validation failed (builtin), keep current file"
      rm -f "$tmp"
      return 1
    fi
    mv "$tmp" "$RULES/custom-hosts.txt"
    log "custom-hosts.txt updated"
    restart_mosdns || true
    return 0
  done
  return 1
}

# 内置最小校验：非注释行第一列必须是域名表达式（不是 IP 形态），其后至少一个合法 IP
valid_custom_hosts() {
  awk '
    /^[[:space:]]*#/ { next }
    /^[[:space:]]*$/ { next }
    {
      if (NF < 2) { printf "line %d: missing IP\n", NR; bad=1; next }
      name=$1
      sub(/^(domain|full|keyword|regexp):/, "", name)
      if (name ~ /^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/) {
        printf "line %d: first column is an IP, format must be "domain IP"\n", NR; bad=1; next
      }
      for (i=2; i<=NF; i++) {
        if ($i !~ /^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/ && $i !~ /^[0-9A-Fa-f:]+$/) {
          printf "line %d: invalid ip %s\n", NR, $i; bad=1
        }
      }
    }
    END { exit bad ? 1 : 0 }
  ' "$1"
}

strip_secret() {
  # 日志不得出现凭据（GS-14）：URL 若带 user:pass@ 则抹掉
  printf '%s' "$1" | sed -E 's#(://)[^/@]+@#\1***@#'
}

sync_loop() {
  rules_interval=${RULES_UPDATE_INTERVAL:-86400}
  next_rules=$(( $(date +%s) + 30 ))   # 启动 30 秒后先同步一次
  while :; do
    now=$(date +%s)
    if [ "$now" -ge "$next_rules" ]; then
      sync_rules_once || true
      next_rules=$(( $(date +%s) + rules_interval ))
    fi
    sync_custom_hosts_once || true
    sleep "$CUSTOM_HOSTS_INTERVAL"
  done
}

# 有完整脚本套件（fetch/validate/apply）时交给 sync-loop.sh（金丝雀/回滚），否则用内置兜底
sync_supervisor() {
  loop="$SCRIPT_DIR/sync-loop.sh"
  [ -x "$loop" ] || loop="/opt/mosdns/sync-loop.sh"
  if [ -x "$loop" ]; then
    log "delegating rule sync to $loop"
    while :; do
      "$loop" || warn "sync-loop.sh exited, restart in 30s"
      sleep 30
    done
  fi
  while :; do
    sync_loop
    warn "sync loop exited, restart in 30s"
    sleep 30
  done
}

shutdown() {
  log "shutdown signal received"
  [ -n "${SYNC_PID:-}" ] && kill "$SYNC_PID" 2>/dev/null || true
  stop_mosdns
  exit 0
}
trap shutdown TERM INT

# ---------------------------------------------------------------- 主流程
init_rules
prepare_optional_files
write_accept_extra
convert_bogus
. "$SCRIPT_DIR/runtime.sh"
RUNTIME_VALIDATE_SCRIPT="$SCRIPT_DIR/validate.sh"
if ! runtime_restore_lkg "$STATE/lkg" "$RULES"; then
  warn "no complete validated LKG snapshot; retaining image/current rules"
  runtime_init_lkg_if_missing "$RULES" "$STATE/lkg" || warn "current rules are not eligible for initial LKG snapshot"
else
  log "restored last known good rules before starting mosdns"
fi
render_config || { warn "config render failed"; exit 1; }
if ! check_config_files; then
  warn "some rule files still missing after init; mosdns may fail to start"
fi

# crond 只服务于旧版 sync-ai，且必须显式启用（D2）
if [ "${ENABLE_ROS_SYNC:-0}" = 1 ]; then
  {
    printf "CONTAINER_DNS=%s\n" "$CONTAINER_DNS"
    printf "*/2 * * * * /sync-ai.sh >/dev/null 2>&1\n"
  } | crontab -
  crond -b -l 2
  log "crond started for deprecated sync-ai (ENABLE_ROS_SYNC=1)"
fi

start_mosdns
sync_supervisor &
SYNC_PID=$!

# 并列监督：mosdns 退出就拉起；收到 TERM 由 trap 处理
MOSDNS_RESTARTS=0
while :; do
  wait "$MOSDNS_PID" 2>/dev/null || true
  MOSDNS_PID=""
  stop_mosdns
  if [ -f "$PIDFILE.reload" ]; then
    rm -f "$PIDFILE.reload"
    MOSDNS_RESTARTS=0
    log "mosdns stopped for a validated rule reload; restarting immediately"
  else
    MOSDNS_RESTARTS=$(( MOSDNS_RESTARTS + 1 ))
    if [ "$MOSDNS_RESTARTS" -gt 1 ]; then
      delay=$(( MOSDNS_RESTARTS * 2 ))
      [ "$delay" -gt 60 ] && delay=60
      warn "mosdns exited, restart #$MOSDNS_RESTARTS in ${delay}s"
      sleep "$delay"
    else
      log "mosdns exited, restarting"
    fi
  fi
  start_mosdns
done

#!/bin/sh
# Independent routing/optional-rule matrix. Run from the tools runner with Docker CLI.
set -u

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
IMAGE=${MOSDNS_IMAGE:-mosdns:p2-tools}
P=mosdns-p2-matrix-$$
NET=$P-net
WORK=$ROOT/tests/.runtime/scenarios-$$
PASS=0
FAIL=0
ACTIVE=0
LAST_QUERY_MS=0
LAST_QUERY_OUT=""

now_ms() {
  if [ -r /proc/uptime ]; then
    awk '{printf "%d", $1*1000}' /proc/uptime
  elif command -v python3 >/dev/null 2>&1; then
    python3 -c 'import time; print(int(time.time()*1000))'
  else
    echo $(($(date +%s) * 1000))
  fi
}

ok() { PASS=$((PASS + 1)); printf 'PASS %s %s\n' "$1" "$2"; }
ng() {
  FAIL=$((FAIL + 1)); printf 'FAIL %s %s\n' "$1" "$2"
  printf 'DIAG %s last_query_ms=%s raw:\n%s\n' "$1" "$LAST_QUERY_MS" "$LAST_QUERY_OUT" >&2
  printf 'DIAG %s SUT logs tail:\n' "$1" >&2
  docker logs "$P-sut" 2>&1 | tail -40 >&2
}
cleanup_case() {
  [ "$ACTIVE" = 1 ] || return 0
  docker rm -fv "$P-sut" "$P-cn" "$P-gl" >/dev/null 2>&1 || true
  ACTIVE=0
}
cleanup() {
  cleanup_case
  docker rm -fv "$P-client" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

if ! command -v docker >/dev/null 2>&1; then
  ng M00 'runner 缺少 docker CLI'; exit 1
fi
if docker network inspect "$NET" >/dev/null 2>&1; then
  ng M00 'matrix network 名称冲突'; exit 1
fi
mkdir -p "$WORK/rules" "$WORK/state" "$WORK/host" "$WORK/baked"
cp "$ROOT"/tests/fixtures/* "$WORK/rules/"
chmod -R 777 "$WORK"
  chmod +x "$ROOT/scripts"/*.sh "$ROOT/tests/mock_upstream.sh"
if ! docker network create --subnet 172.31.251.0/24 "$NET" >/dev/null 2>&1; then
  ng M00 '无法创建独立 matrix 网络'; exit 1
fi

start_mock() {
  name=$1; ip=$2
  addr=172.31.251.2; [ "$name" = "$P-gl" ] && addr=172.31.251.3
  docker run -d --name "$name" --network "$NET" --ip "$addr" --entrypoint /opt/tests/mock_upstream.sh \
    -v "$ROOT/tests:/opt/tests:ro" "$IMAGE" "$ip" 53 >/dev/null 2>&1
}
start_silent() {
  name=$1
  addr=172.31.251.2; [ "$name" = "$P-gl" ] && addr=172.31.251.3
  # Listen on UDP/53 and discard packets: this is a real no-response timeout,
  # unlike an unbound port which can return an immediate ICMP refusal.
  docker run -d --name "$name" --network "$NET" --ip "$addr" --entrypoint /bin/sh "$IMAGE" \
    -c 'while :; do nc -u -l -p 53 >/dev/null 2>&1; done' >/dev/null 2>&1
}
start_case() {
  cn_ip=$1; gl_ip=$2; ipv6=${3:-0}; optional=${4:-normal}; threshold=${5:-}
  cleanup_case
  rm -rf "$WORK/rules" "$WORK/state" "$WORK/host"
  mkdir -p "$WORK/rules" "$WORK/state" "$WORK/host" "$WORK/baked"
  cp "$ROOT"/tests/fixtures/* "$WORK/rules/"
  case "$optional" in
    missing) rm -f "$WORK/rules/cn-exceptions.txt" "$WORK/rules/apple-cn.txt" ;;
    empty) : > "$WORK/rules/cn-exceptions.txt"; : > "$WORK/rules/apple-cn.txt" ;;
  esac
  chmod -R 777 "$WORK"
  chmod +x "$ROOT/scripts"/*.sh "$ROOT/tests/mock_upstream.sh"
  if [ "$cn_ip" = SILENT ]; then start_silent "$P-cn" || return 1
  else start_mock "$P-cn" "$cn_ip" || return 1; fi
  if [ "$gl_ip" = SILENT ]; then start_silent "$P-gl" || return 1
  else start_mock "$P-gl" "$gl_ip" || return 1; fi
  # mosdns parses upstream addr as a literal IP. Stable IPs avoid service-name DNS
  # and Docker CLI inspect formatting differences.
  # FALLBACK_STANDBY 必须 false：always_standby 时 fallback 在 primary 成功后仍投递
  # secondary，先入 respChan 的非空应答获胜，M01 会偶发拿到 global。
  if ! docker run -d --name "$P-sut" --network "$NET" \
    -v "$ROOT/scripts:/opt/mosdns:ro" -v "$ROOT/tests:/opt/tests:ro" \
    -v "$WORK/rules:/etc/mosdns/rules" -v "$WORK/state:/data/state" \
    -v "$WORK/host:/etc/mosdns/host" -v "$WORK/baked:/opt/mosdns-rules" \
    -e CONTAINER_DNS=223.5.5.5 -e DNS_CN=172.31.251.2 -e DNS_GLOBAL=172.31.251.3 \
    -e RULES_SOURCES= -e CUSTOM_HOSTS_SOURCES= -e RULES_UPDATE_INTERVAL=86400 \
    -e ENABLE_IPV6="$ipv6" -e FALLBACK_THRESHOLD="${threshold:-300}" \
    -e FALLBACK_STANDBY=false \
    --entrypoint /opt/mosdns/entrypoint.sh "$IMAGE" >/dev/null 2>&1; then
    docker rm -fv "$P-cn" "$P-gl" >/dev/null 2>&1 || true
    return 1
  fi
  ACTIVE=1
  for role in cn gl; do
    case "$role" in
      cn) value=$cn_ip; upstream=172.31.251.2 ;;
      gl) value=$gl_ip; upstream=172.31.251.3 ;;
    esac
    [ "$value" = SILENT ] && continue
    probe_type=A; case "$value" in *:*) probe_type=AAAA ;; esac
    attempt=0
    ready_up=0
    while [ "$attempt" -lt 40 ]; do
      if docker exec "$P-sut" dig @"$upstream" upstream-ready.test "$probe_type" +time=1 +tries=1 +short 2>/dev/null | grep -Fxq "$value"; then
        ready_up=1
        break
      fi
      sleep 0.1
      attempt=$((attempt + 1))
    done
    [ "$ready_up" = 1 ] || return 1
  done
  i=0
  while [ "$i" -lt 40 ]; do
    if docker exec "$P-sut" dig @127.0.0.1 -p 53 pin.test A +tries=1 +time=1 +short 2>/dev/null | grep -Fxq 192.0.2.10; then
      return 0
    fi
    sleep 0.5
    i=$((i + 1))
  done
  return 1
}
query_for() {
  domain=$1; type=${2:-A}; limit=${3:-1}
  t0=$(now_ms)
  LAST_QUERY_OUT=$(docker exec "$P-sut" dig @127.0.0.1 -p 53 "$domain" "$type" +tries=1 +time="$limit" +noall +comments +answer 2>&1)
  t1=$(now_ms)
  LAST_QUERY_MS=$((t1 - t0))
  printf 'DIAG query domain=%s type=%s time=%sms\n%s\n' "$domain" "$type" "$LAST_QUERY_MS" "$LAST_QUERY_OUT" >&2
  printf '%s\n' "$LAST_QUERY_OUT"
}
query() { query_for "$1" "${2:-A}" 6; }
answer_for() { query_for "$1" "${2:-A}" "${3:-1}" | awk '!/^;/ && /[[:space:]]IN[[:space:]]A[[:space:]]/ {print $NF; exit}'; }
rcode_for() { query_for "$1" "${2:-A}" "${3:-1}" | sed -n 's/.*status: \([^,]*\),.*/\1/p' | head -n 1; }
answer() { query "$1" "${2:-A}" | awk '!/^;/ && /[[:space:]]IN[[:space:]]A[[:space:]]/ {print $NF; exit}'; }
rcode() { query "$1" "${2:-A}" | sed -n 's/.*status: \([^,]*\),.*/\1/p' | head -n 1; }
query_count() {
  docker logs "$1" 2>&1 | grep -F "$2" | grep -F 'mock_query' | wc -l | tr -d ' '
}
ready_case() {
  if start_case "$1" "$2" "${3:-0}" "${4:-normal}" "${6:-}"; then return 0; fi
  ng "$5" '被测实例未在超时内就绪'
  cleanup_case
  return 1
}

# Unknown domain outcomes (one isolated SUT per upstream response pair).
if ready_case 192.0.2.1 198.51.100.1 0 normal M01 2000; then
  got=$(answer unknown-cn.matrix.test)
  if [ "$got" = 192.0.2.1 ]; then
    ok M01 '未知域名 CN 正常应答被接受'
  else
    cn_n=$(query_count "$P-cn" unknown-cn.matrix.test)
    gl_n=$(query_count "$P-gl" unknown-cn.matrix.test)
    ng M01 "未知域名 CN 应答期望 192.0.2.1，实际 $got cn=$cn_n global=$gl_n"
  fi
fi
if ready_case 9.9.9.9 198.51.100.1 0 normal M02; then
  got=$(answer unknown-noncn.matrix.test)
  [ "$got" = 198.51.100.1 ] && ok M02 '未知域名 CN 非接受集应答转 global' || ng M02 "CN 非接受集应答期望 global 198.51.100.1，实际 $got"
fi
if ready_case 192.0.2.99 198.51.100.1 0 normal M03; then
  got=$(answer unknown-bogus.matrix.test)
  [ "$got" = 198.51.100.1 ] && ok M03 'bogus CN 应答先丢弃再取 global' || ng M03 "bogus 应答期望 198.51.100.1，实际 $got"
fi
if ready_case SILENT 198.51.100.1 0 normal M04; then
  got=$(answer unknown-cn-timeout.matrix.test)
  code=$(rcode unknown-cn-timeout.matrix.test)
  [ "$got" = 198.51.100.1 ] && [ "$code" = NOERROR ] && ok M04 'CN 上游无响应后以 NOERROR/global 应答' || ng M04 "CN 无响应期望 NOERROR/198.51.100.1，实际 rcode=$code answer=$got"
fi
if ready_case 192.0.2.1 SILENT 0 normal M05; then
  got=$(rcode_for google.test A 6)
  [ "$got" = SERVFAIL ] && ok M05 '硬名单 global 上游无响应返回 SERVFAIL' || ng M05 "global 无响应期望 SERVFAIL，实际 $got"
fi

# Additional accepted/rejected address classes.
if ready_case 172.16.8.50 198.51.100.1 0 normal M06 2000; then
  got=$(answer unknown-private.matrix.test)
  [ "$got" = 172.16.8.50 ] && ok M06 'RFC1918 私网应答被接受' || ng M06 "私网应答期望 172.16.8.50，实际 $got"
fi
if ready_case 100.64.1.2 198.51.100.1 0 normal M07 2000; then
  got=$(answer unknown-cgnat.matrix.test)
  [ "$got" = 100.64.1.2 ] && ok M07 'CGNAT 应答被接受' || ng M07 "CGNAT 应答期望 100.64.1.2，实际 $got"
fi
if ready_case 127.0.0.1 198.51.100.1 0 normal M08; then
  got=$(answer unknown-loopback.matrix.test)
  [ "$got" = 198.51.100.1 ] && ok M08 'loopback 应答丢弃并转 global' || ng M08 "loopback 丢弃后期望 198.51.100.1，实际 $got"
fi

# A non-accepted cn-exception response drops, then the enclosing hard-list rule runs.
if ready_case 198.51.100.2 198.51.100.1 0 normal M09; then
  got=$(answer_for fonts.hard.test A 6)
  cn_n=$(query_count "$P-cn" fonts.hard.test); gl_n=$(query_count "$P-gl" fonts.hard.test)
  if [ "$got" = 198.51.100.1 ] && [ "$cn_n" -ge 1 ] && [ "$gl_n" -ge 1 ]; then
    ok M09 'cn_exceptions 非接受应答丢弃后落硬名单，CN 与 global 均命中'
  else
    ng M09 "cn_exceptions 落硬名单结果/计数不符 answer=$got cn=$cn_n global=$gl_n"
  fi
fi
if ready_case 192.0.2.1 198.51.100.1 0 normal M10 2000; then
  got=$(answer soft.test)
  cn_n=$(query_count "$P-cn" soft.test); gl_n=$(query_count "$P-gl" soft.test)
  if [ "$got" = 192.0.2.1 ] && [ "$cn_n" -ge 1 ]; then
    ok M10 '软名单 goto D3，CN 应答被接受'
  else
    ng M10 "软名单 goto 结果/计数不符 answer=$got cn=$cn_n global=$gl_n"
  fi
fi

# ENABLE_IPV6 must remove the empty-AAAA branch and reach the upstream.
if ready_case 192.0.2.1 2001:db8::1 1 normal M11; then
  got=$(query unknown-ipv6.matrix.test AAAA)
  if printf '%s\n' "$got" | grep -Fq '2001:db8::1'; then
    ok M11 'ENABLE_IPV6=1 加载成功且 AAAA 返回上游记录'
  else
    ng M11 "ENABLE_IPV6 AAAA 未命中上游: $(printf '%s' "$got" | tr '\n' ' ')"
  fi
fi

# Optional cn-exceptions/apple files: both missing and empty should only warn.
if ready_case 192.0.2.1 198.51.100.1 0 missing M12; then
  logs=$(docker logs "$P-sut" 2>&1)
  if printf '%s\n' "$logs" | grep -Fq 'optional rule cn-exceptions.txt is missing' \
    && printf '%s\n' "$logs" | grep -Fq 'optional rule apple-cn.txt is missing'; then
    ok M12 'cn-exceptions/apple-cn 缺失只告警且正常啟動'
  else ng M12 '缺失的可选名单告警/启动行为不符'; fi
fi
if ready_case 192.0.2.1 198.51.100.1 0 empty M13; then
  logs=$(docker logs "$P-sut" 2>&1)
  if printf '%s\n' "$logs" | grep -Fq 'optional rule cn-exceptions.txt is empty' \
    && printf '%s\n' "$logs" | grep -Fq 'optional rule apple-cn.txt is empty'; then
    ok M13 'cn-exceptions/apple-cn 空文件只告警且正常啟動'
  else ng M13 '空的可选名单告警/启动行为不符'; fi
fi

printf 'MATRIX PASS=%s FAIL=%s\n' "$PASS" "$FAIL"
exit "$FAIL"

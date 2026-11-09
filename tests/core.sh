#!/bin/sh
# Isolated core-feature tests. Names contain "core"; subnet is not 172.31.250.0/24.
set -u

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
IMAGE=${MOSDNS_IMAGE:-mosdns:p2-tools}
P=mosdns-core-$$
NET=$P-net
WORK=$ROOT/tests/.runtime/core-$$
PASS=0
FAIL=0
ACTIVE=0

ok() { PASS=$((PASS + 1)); printf 'PASS %s %s\n' "$1" "$2"; }
ng() { FAIL=$((FAIL + 1)); printf 'FAIL %s %s\n' "$1" "$2"; }

cleanup_case() {
  docker rm -fv "$P-sut" >/dev/null 2>&1 || true
  ACTIVE=0
}
cleanup() {
  cleanup_case
  docker rm -fv "$P-cn" "$P-gl" "$P-ai" "$P-dead" "$P-ros" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

if ! command -v docker >/dev/null 2>&1; then
  ng C00 'runner 缺少 docker CLI'; exit 1
fi
if docker network inspect "$NET" >/dev/null 2>&1; then
  ng C00 'core network 名称冲突'; exit 1
fi
mkdir -p "$WORK/rules" "$WORK/state" "$WORK/host"
cp "$ROOT"/tests/fixtures/* "$WORK/rules/"
chmod -R 777 "$WORK"
chmod +x "$ROOT/scripts"/*.sh "$ROOT/tests/mock_upstream.sh" "$ROOT/tests/mock_ros.sh"
if ! docker network create --subnet 172.31.240.0/24 "$NET" >/dev/null 2>&1; then
  ng C00 '无法创建 core 测试网络'; exit 1
fi

CN_ADDR=172.31.240.2
GL_ADDR=172.31.240.3
AI_ADDR=172.31.240.4
DEAD_ADDR=172.31.240.5
ROS_ADDR=172.31.240.10

start_mock() {
  name=$1; ip=$2; addr=$3
  docker run -d --name "$name" --network "$NET" --ip "$addr" --entrypoint /opt/tests/mock_upstream.sh \
    -v "$ROOT/tests:/opt/tests:ro" "$IMAGE" "$ip" 53 >/dev/null 2>&1
}
start_silent() {
  name=$1; addr=$2
  docker run -d --name "$name" --network "$NET" --ip "$addr" --entrypoint /bin/sh "$IMAGE" \
    -c 'while :; do nc -u -l -p 53 >/dev/null 2>&1; done' >/dev/null 2>&1
}

# Extra docker run args after the image are not used; env is passed via the env file.
start_sut() {
  envfile=$1
  cleanup_case
  rm -rf "$WORK/rules" "$WORK/state" "$WORK/host"
  mkdir -p "$WORK/rules" "$WORK/state" "$WORK/host"
  cp "$ROOT"/tests/fixtures/* "$WORK/rules/"
  chmod -R 777 "$WORK"
  chmod +x "$ROOT/scripts"/*.sh
  if ! docker run -d --name "$P-sut" --network "$NET" \
    -v "$ROOT/scripts:/opt/mosdns:ro" -v "$ROOT/tests:/opt/tests:ro" \
    -v "$WORK/rules:/etc/mosdns/rules" -v "$WORK/state:/data/state" \
    -v "$WORK/host:/etc/mosdns/host" \
    --env-file "$envfile" \
    --entrypoint /opt/mosdns/entrypoint.sh "$IMAGE" >/dev/null 2>&1; then
    return 1
  fi
  ACTIVE=1
  i=0
  while [ "$i" -lt 20 ]; do
    if docker exec "$P-sut" dig @127.0.0.1 -p 53 pin.test A +tries=1 +time=1 +short 2>/dev/null | grep -Fxq 192.0.2.10; then
      return 0
    fi
    sleep 0.5
    i=$((i + 1))
  done
  return 1
}

write_env() {
  dest=$1
  cat > "$dest" <<EOF
DNS_CN=$CN_ADDR
DNS_GLOBAL=$GL_ADDR
RULES_SOURCES=
CUSTOM_HOSTS_SOURCES=
RULES_UPDATE_INTERVAL=86400
CANARY_SENTINEL=pin.test
REL_FLOOR_PCT=0
EOF
}

if ! start_mock "$P-cn" 192.0.2.1 "$CN_ADDR"; then ng C00 'CN mock 启动失败'; exit 1; fi
if ! start_mock "$P-gl" 198.51.100.1 "$GL_ADDR"; then ng C00 'GLOBAL mock 启动失败'; exit 1; fi

# --- C01: 未设置 CONTAINER_DNS 时默认 119.29.29.29，DNS 正常 ---
write_env "$WORK/env.c01"
if start_sut "$WORK/env.c01"; then
  resolv=$(docker exec "$P-sut" cat /etc/resolv.conf 2>/dev/null || true)
  got=$(docker exec "$P-sut" dig @127.0.0.1 -p 53 pin.test A +tries=1 +time=1 +short 2>/dev/null | head -n 1)
  logs=$(docker logs "$P-sut" 2>&1 || true)
  if printf '%s\n' "$resolv" | grep -Fq 'nameserver 119.29.29.29' \
    && [ "$got" = 192.0.2.10 ] \
    && ! printf '%s\n' "$logs" | grep -Ei 'ERROR|error' | grep -Fq 'CONTAINER_DNS'; then
    ok C01 '未设置 CONTAINER_DNS 时默认 119.29.29.29 且 DNS 正常'
  else
    ng C01 "默认值/解析不符 resolv=$(printf '%s' "$resolv" | tr '\n' ' ') answer=$got"
  fi
else
  ng C01 '被测实例未就绪'
  docker logs "$P-sut" 2>&1 | tail -20
fi

# --- C02: 显式空字符串同样回退默认，不报错 ---
write_env "$WORK/env.c02"
printf 'CONTAINER_DNS=\n' >> "$WORK/env.c02"
if start_sut "$WORK/env.c02"; then
  resolv=$(docker exec "$P-sut" cat /etc/resolv.conf 2>/dev/null || true)
  got=$(docker exec "$P-sut" dig @127.0.0.1 -p 53 pin.test A +tries=1 +time=1 +short 2>/dev/null | head -n 1)
  logs=$(docker logs "$P-sut" 2>&1 || true)
  if printf '%s\n' "$resolv" | grep -Fq 'nameserver 119.29.29.29' \
    && [ "$got" = 192.0.2.10 ] \
    && ! printf '%s\n' "$logs" | grep -Ei 'WARN|ERROR|error' | grep -Fq 'CONTAINER_DNS'; then
    ok C02 'CONTAINER_DNS 显式为空时回退默认且无告警'
  else
    ng C02 "空值回退不符 resolv=$(printf '%s' "$resolv" | tr '\n' ' ') answer=$got"
  fi
else
  ng C02 '被测实例未就绪'
  docker logs "$P-sut" 2>&1 | tail -20
fi

# --- U01: DNS_GLOBAL 两个上游（一个不响应）时硬名单查询仍成功，默认 concurrent=2 ---
if ! start_silent "$P-dead" "$DEAD_ADDR"; then
  ng U01 '静默上游启动失败'
else
  write_env "$WORK/env.u01"
  printf 'DNS_GLOBAL=%s,%s\n' "$GL_ADDR" "$DEAD_ADDR" >> "$WORK/env.u01"
  if start_sut "$WORK/env.u01"; then
    cfg=$(docker exec "$P-sut" cat /etc/mosdns/config.yaml 2>/dev/null || true)
    conc=$(printf '%s\n' "$cfg" | awk '/tag: forward_global/{f=1} f&&/concurrent:/{print $2; exit}')
    has_gl=$(printf '%s\n' "$cfg" | grep -c "addr: $GL_ADDR" || true)
    has_dead=$(printf '%s\n' "$cfg" | grep -c "addr: $DEAD_ADDR" || true)
    got=$(docker exec "$P-sut" dig @127.0.0.1 -p 53 google.test A +tries=2 +time=5 +short 2>/dev/null | head -n 1)
    if [ "$conc" = 2 ] && [ "$has_gl" -ge 1 ] && [ "$has_dead" -ge 1 ] && [ "$got" = 198.51.100.1 ]; then
      ok U01 'DNS_GLOBAL 双上游且其一不响应时查询仍成功，默认 concurrent=2'
    else
      ng U01 "双上游结果不符 concurrent=$conc addrs=$has_gl/$has_dead answer=$got"
    fi
  else
    ng U01 '被测实例未就绪'
    docker logs "$P-sut" 2>&1 | tail -20
  fi
fi

# --- U02: 显式 CONCURRENT_* 超出范围夹到 1..3 ---
write_env "$WORK/env.u02"
printf 'DNS_GLOBAL=%s,%s\nCONCURRENT_GLOBAL=99\nCONCURRENT_CN=0\nCONCURRENT_AI=2\n' "$GL_ADDR" "$DEAD_ADDR" >> "$WORK/env.u02"
if start_sut "$WORK/env.u02"; then
  cfg=$(docker exec "$P-sut" cat /etc/mosdns/config.yaml 2>/dev/null || true)
  g=$(printf '%s\n' "$cfg" | awk '/tag: forward_global/{f=1} f&&/concurrent:/{print $2; exit}')
  c=$(printf '%s\n' "$cfg" | awk '/tag: forward_cn/{f=1} f&&/concurrent:/{print $2; exit}')
  a=$(printf '%s\n' "$cfg" | awk '/tag: forward_ai/{f=1} f&&/concurrent:/{print $2; exit}')
  got=$(docker exec "$P-sut" dig @127.0.0.1 -p 53 google.test A +tries=2 +time=5 +short 2>/dev/null | head -n 1)
  if [ "$g" = 3 ] && [ "$c" = 1 ] && [ "$a" = 2 ] && [ "$got" = 198.51.100.1 ]; then
    ok U02 'CONCURRENT_* 显式值夹到 1..3 且查询成功'
  else
    ng U02 "夹取不符 CN=$c GLOBAL=$g AI=$a answer=$got"
  fi
else
  ng U02 '被测实例未就绪'
  docker logs "$P-sut" 2>&1 | tail -20
fi

query_count() {
  docker logs "$1" 2>&1 | grep -F "$2" | grep -F 'mock_query' | wc -l | tr -d ' '
}

# --- A01: DNS_AI 独立 mock 时 AI 域名只打到 AI 上游 ---
if ! start_mock "$P-ai" 203.0.113.1 "$AI_ADDR"; then
  ng A01 'AI mock 启动失败'
else
  write_env "$WORK/env.a01"
  printf 'DNS_AI=%s\n' "$AI_ADDR" >> "$WORK/env.a01"
  if start_sut "$WORK/env.a01"; then
    gl_before=$(query_count "$P-gl" 'ai.test')
    got=$(docker exec "$P-sut" dig @127.0.0.1 -p 53 ai.test A +tries=1 +time=2 +short 2>/dev/null | head -n 1)
    ai_n=$(query_count "$P-ai" 'ai.test')
    gl_n=$(query_count "$P-gl" 'ai.test')
    gl_hit=$((gl_n - gl_before))
    if [ "$got" = 203.0.113.1 ] && [ "$ai_n" -ge 1 ] && [ "$gl_hit" -eq 0 ]; then
      ok A01 '设置 DNS_AI 时 AI 域名只打到 AI 上游'
    else
      ng A01 "AI 分流不符 answer=$got ai=$ai_n global_delta=$gl_hit"
    fi
  else
    ng A01 '被测实例未就绪'
    docker logs "$P-sut" 2>&1 | tail -20
  fi
fi

# --- A02: 同时在 gfw 名单里的 AI 域名也走 AI ---
write_env "$WORK/env.a02"
printf 'DNS_AI=%s\n' "$AI_ADDR" >> "$WORK/env.a02"
if start_sut "$WORK/env.a02"; then
  gl_before=$(query_count "$P-gl" 'ai-gfw.test')
  got=$(docker exec "$P-sut" dig @127.0.0.1 -p 53 ai-gfw.test A +tries=1 +time=2 +short 2>/dev/null | head -n 1)
  ai_n=$(query_count "$P-ai" 'ai-gfw.test')
  gl_n=$(query_count "$P-gl" 'ai-gfw.test')
  gl_hit=$((gl_n - gl_before))
  if [ "$got" = 203.0.113.1 ] && [ "$ai_n" -ge 1 ] && [ "$gl_hit" -eq 0 ]; then
    ok A02 '同时在 gfw 名单的 AI 域名仍走 AI 上游'
  else
    ng A02 "AI+gfw 分流不符 answer=$got ai=$ai_n global_delta=$gl_hit"
  fi
else
  ng A02 '被测实例未就绪'
  docker logs "$P-sut" 2>&1 | tail -20
fi

# --- A03: 未设 DNS_AI 时 AI 域名走 global ---
write_env "$WORK/env.a03"
if start_sut "$WORK/env.a03"; then
  gl_before=$(query_count "$P-gl" 'ai.test')
  ai_before=$(query_count "$P-ai" 'ai.test')
  got=$(docker exec "$P-sut" dig @127.0.0.1 -p 53 ai.test A +tries=1 +time=2 +short 2>/dev/null | head -n 1)
  gl_n=$(query_count "$P-gl" 'ai.test')
  ai_n=$(query_count "$P-ai" 'ai.test')
  gl_hit=$((gl_n - gl_before))
  ai_hit=$((ai_n - ai_before))
  if [ "$got" = 198.51.100.1 ] && [ "$gl_hit" -ge 1 ] && [ "$ai_hit" -eq 0 ]; then
    ok A03 '未设 DNS_AI 时 AI 域名走 global'
  else
    ng A03 "默认 AI=global 不符 answer=$got global_delta=$gl_hit ai_delta=$ai_hit"
  fi
else
  ng A03 '被测实例未就绪'
  docker logs "$P-sut" 2>&1 | tail -20
fi

sut_ps() { docker exec "$P-sut" ps 2>/dev/null || true; }
has_proc() { sut_ps | grep -v grep | grep -Fq "$1"; }

# --- R01: ENABLE_ROS_SYNC=0 且 ROS_* 全空：无同步进程、无错误告警、DNS 正常 ---
write_env "$WORK/env.r01"
if start_sut "$WORK/env.r01"; then
  sleep 1
  logs=$(docker logs "$P-sut" 2>&1 || true)
  got=$(docker exec "$P-sut" dig @127.0.0.1 -p 53 pin.test A +tries=1 +time=1 +short 2>/dev/null | head -n 1)
  if [ "$got" = 192.0.2.10 ] \
    && ! has_proc sync-ai \
    && ! has_proc crond \
    && ! printf '%s\n' "$logs" | grep -Eq 'ERROR ENABLE_ROS_SYNC|WARN.*ROS_|crond started|sync-ai'; then
    ok R01 'ENABLE_ROS_SYNC=0 时无同步进程、无 ROS 报错、DNS 正常'
  else
    ng R01 "关闭同步语义不符 answer=$got ps=$(sut_ps | tr '\n' ';')"
  fi
else
  ng R01 '被测实例未就绪'
  docker logs "$P-sut" 2>&1 | tail -20
fi

# --- R02: ENABLE_ROS_SYNC=1 缺 ROS_PASS：一条错误日志、不启同步、DNS 正常 ---
write_env "$WORK/env.r02"
printf 'ENABLE_ROS_SYNC=1\nROS_HOST=192.0.2.8\nROS_USER=admin\nROS_PASS=\n' >> "$WORK/env.r02"
if start_sut "$WORK/env.r02"; then
  sleep 1
  logs=$(docker logs "$P-sut" 2>&1 || true)
  got=$(docker exec "$P-sut" dig @127.0.0.1 -p 53 pin.test A +tries=1 +time=1 +short 2>/dev/null | head -n 1)
  err=$(printf '%s\n' "$logs" | grep -c 'ERROR ENABLE_ROS_SYNC=1 但缺少 ROS_HOST 或 ROS_PASS' || true)
  if [ "$got" = 192.0.2.10 ] && [ "$err" = 1 ] && ! has_proc sync-ai && ! has_proc crond; then
    ok R02 'ENABLE_ROS_SYNC=1 缺 ROS_PASS 时一条错误且 DNS 正常'
  else
    ng R02 "缺凭据语义不符 answer=$got err=$err ps=$(sut_ps | tr '\n' ';')"
  fi
else
  ng R02 '被测实例未就绪'
  docker logs "$P-sut" 2>&1 | tail -20
fi

start_ros() {
  mkdir -p "$WORK/ros"
  chmod 777 "$WORK/ros"
  : > "$WORK/ros/http.log"
  : > "$WORK/ros/added.log"
  docker rm -fv "$P-ros" >/dev/null 2>&1 || true
  docker run -d --name "$P-ros" --network "$NET" --ip "$ROS_ADDR" \
    -v "$ROOT/tests:/opt/tests:ro" -v "$WORK/ros:/tmp/mock-ros" \
    -e ROS_USER=admin -e ROS_PASS=testpass \
    --entrypoint /opt/tests/mock_ros.sh "$IMAGE" 80 >/dev/null 2>&1
}

# --- R03: mock ROS 验证 GET 过滤、remove、add 请求体 ---
if ! start_ros; then
  ng R03 'mock ROS 启动失败'
else
  write_env "$WORK/env.r03"
  cat >> "$WORK/env.r03" <<EOF
ENABLE_ROS_SYNC=1
ROS_HOST=$ROS_ADDR
ROS_USER=admin
ROS_PASS=testpass
SYNC_AI_DNS=127.0.0.1
SYNC_AI_INTERVAL=2
ROS_AI_LIST=ai-sgp
ROS_AI_TTL=1800s
ROS_AI_COMMENT=mosdns-ai
EOF
  if start_sut "$WORK/env.r03"; then
    i=0
    while [ "$i" -lt 15 ]; do
      grep -q 'add list=ai-sgp' "$WORK/ros/added.log" 2>/dev/null && break
      sleep 1
      i=$((i + 1))
    done
    got=$(docker exec "$P-sut" dig @127.0.0.1 -p 53 pin.test A +tries=1 +time=1 +short 2>/dev/null | head -n 1)
    logs=$(docker logs "$P-sut" 2>&1 || true)
    http=$(cat "$WORK/ros/http.log" 2>/dev/null || true)
    added=$(cat "$WORK/ros/added.log" 2>/dev/null || true)
    has_get=0; printf '%s\n' "$http" | grep -Fq 'GET /rest/ip/firewall/address-list?list=ai-sgp&comment=mosdns-ai' && has_get=1
    has_del=0; printf '%s\n' "$added" | grep -Fq 'remove *1' && has_del=1
    has_add=0; printf '%s\n' "$added" | grep -Fq 'add list=ai-sgp address=198.51.100.1 timeout=1800s comment=mosdns-ai' && has_add=1
    started=0; printf '%s\n' "$logs" | grep -Fq 'sync-ai 后台循环已启动 interval=2s' && started=1
    if [ "$got" = 192.0.2.10 ] && [ "$has_get" = 1 ] && [ "$has_del" = 1 ] && [ "$has_add" = 1 ] && [ "$started" = 1 ]; then
      ok R03 'mock ROS 上 GET 过滤、remove、add 请求体正确且 DNS 正常'
    else
      ng R03 "ROS 同步请求不符 get=$has_get del=$has_del add=$has_add started=$started answer=$got"
      printf '%s\n' "$http" | tail -20
      printf '%s\n' "$added"
    fi
    # --- R05: 成功路径日志无凭据 ---
    b64=$(printf 'admin:testpass' | base64 | tr -d '\n')
    if printf '%s\n' "$logs" | grep -Fq 'testpass' \
      || printf '%s\n' "$logs" | grep -Fq "$b64" \
      || printf '%s\n' "$logs" | grep -Fiq 'Authorization'; then
      ng R05 '日志泄露了 ROS_PASS 或 Authorization'
    else
      ok R05 '日志未打印 ROS_PASS 或 Authorization'
    fi
  else
    ng R03 '被测实例未就绪'
    ng R05 '被测实例未就绪，跳过凭据日志检查'
    docker logs "$P-sut" 2>&1 | tail -20
  fi
fi

# --- R04: 凭据错误只告警，DNS 不受影响 ---
write_env "$WORK/env.r04"
cat >> "$WORK/env.r04" <<EOF
ENABLE_ROS_SYNC=1
ROS_HOST=$ROS_ADDR
ROS_USER=admin
ROS_PASS=wrongpass
SYNC_AI_DNS=127.0.0.1
SYNC_AI_INTERVAL=2
EOF
: > "$WORK/ros/added.log"
: > "$WORK/ros/http.log"
if start_sut "$WORK/env.r04"; then
  sleep 3
  got=$(docker exec "$P-sut" dig @127.0.0.1 -p 53 pin.test A +tries=1 +time=1 +short 2>/dev/null | head -n 1)
  logs=$(docker logs "$P-sut" 2>&1 || true)
  added=$(cat "$WORK/ros/added.log" 2>/dev/null || true)
  warn_auth=0; printf '%s\n' "$logs" | grep -Fq 'RouterOS 认证失败' && warn_auth=1
  leaked=0
  printf '%s\n' "$logs" | grep -Fq 'wrongpass' && leaked=1
  printf '%s\n' "$logs" | grep -Fiq 'Authorization' && leaked=1
  if [ "$got" = 192.0.2.10 ] && [ "$warn_auth" = 1 ] && [ "$leaked" = 0 ] && ! printf '%s\n' "$added" | grep -q '^add '; then
    ok R04 '凭据错误时只告警、不写 ROS、DNS 不受影响'
  else
    ng R04 "错误凭据行为不符 answer=$got warn=$warn_auth leaked=$leaked added=$(printf '%s' "$added" | tr '\n' ';')"
  fi
else
  ng R04 '被测实例未就绪'
  docker logs "$P-sut" 2>&1 | tail -20
fi

# --- E01: --env-file .env.example 变量值干净且 DNS 正常 ---
cleanup_case
rm -rf "$WORK/rules" "$WORK/state" "$WORK/host"
mkdir -p "$WORK/rules" "$WORK/state" "$WORK/host"
cp "$ROOT"/tests/fixtures/* "$WORK/rules/"
chmod -R 777 "$WORK"
if docker run -d --name "$P-sut" --network "$NET" \
  -v "$ROOT/scripts:/opt/mosdns:ro" -v "$ROOT/tests:/opt/tests:ro" \
  -v "$WORK/rules:/etc/mosdns/rules" -v "$WORK/state:/data/state" \
  -v "$WORK/host:/etc/mosdns/host" \
  --env-file "$ROOT/.env.example" \
  -e "DNS_CN=$CN_ADDR" -e "DNS_GLOBAL=$GL_ADDR" \
  -e RULES_SOURCES= -e CUSTOM_HOSTS_SOURCES= \
  -e RULES_UPDATE_INTERVAL=86400 -e CANARY_SENTINEL=pin.test -e REL_FLOOR_PCT=0 \
  --entrypoint /opt/mosdns/entrypoint.sh "$IMAGE" >/dev/null 2>&1; then
  ACTIVE=1
  i=0
  ready=0
  while [ "$i" -lt 20 ]; do
    if docker exec "$P-sut" dig @127.0.0.1 -p 53 pin.test A +tries=1 +time=1 +short 2>/dev/null | grep -Fxq 192.0.2.10; then
      ready=1; break
    fi
    sleep 0.5
    i=$((i + 1))
  done
  envdump=$(docker exec "$P-sut" printenv 2>/dev/null || true)
  logs=$(docker logs "$P-sut" 2>&1 || true)
  dirty=$(printf '%s\n' "$envdump" | grep '#' || true)
  auth=$(docker exec "$P-sut" sh -c 'printenv CUSTOM_HOSTS_AUTH || true' 2>/dev/null)
  ros=$(docker exec "$P-sut" sh -c 'printenv ENABLE_ROS_SYNC || true' 2>/dev/null)
  cdns=$(docker exec "$P-sut" sh -c 'printenv CONTAINER_DNS || true' 2>/dev/null)
  conc=$(docker exec "$P-sut" sh -c 'printenv CONCURRENT_CN || true' 2>/dev/null)
  got=$(docker exec "$P-sut" dig @127.0.0.1 -p 53 pin.test A +tries=1 +time=1 +short 2>/dev/null | head -n 1)
  if [ "$ready" = 1 ] && [ "$got" = 192.0.2.10 ] && [ -z "$dirty" ] && [ -z "$auth" ] \
    && [ "$ros" = 0 ] && [ "$cdns" = 119.29.29.29 ] && [ -z "$conc" ] \
    && ! printf '%s\n' "$logs" | grep -Eq 'ERROR ENABLE_ROS_SYNC|sync-ai 后台循环' \
    && ! has_proc sync-ai && ! has_proc crond; then
    ok E01 '--env-file .env.example 变量值干净且 DNS 正常'
  else
    ng E01 "env-file 用例不符 ready=$ready answer=$got ros=$ros cdns=$cdns conc=$conc dirty=$(printf '%s' "$dirty" | tr '\n' ' ') auth=$(printf '%s' "$auth" | tr '\n' ' ')"
  fi
else
  ng E01 '使用 .env.example 启动失败'
fi

printf 'CORE PASS=%s FAIL=%s\n' "$PASS" "$FAIL"
exit "$FAIL"

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
  [ "$ACTIVE" = 1 ] || return 0
  docker rm -fv "$P-sut" "$P-cn" "$P-gl" "$P-ai" "$P-dead" >/dev/null 2>&1 || true
  ACTIVE=0
}
cleanup() {
  cleanup_case
  docker rm -fv "$P-ros" >/dev/null 2>&1 || true
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
chmod +x "$ROOT/scripts"/*.sh "$ROOT/tests/mock_upstream.sh"
if ! docker network create --subnet 172.31.240.0/24 "$NET" >/dev/null 2>&1; then
  ng C00 '无法创建 core 测试网络'; exit 1
fi

CN_ADDR=172.31.240.2
GL_ADDR=172.31.240.3
AI_ADDR=172.31.240.4
DEAD_ADDR=172.31.240.5

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

printf 'CORE PASS=%s FAIL=%s\n' "$PASS" "$FAIL"
exit "$FAIL"

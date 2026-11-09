#!/bin/sh
# 总验收入口；必须在带 docker CLI、dig、curl 的容器里用 BusyBox sh 执行。
set -u

ROOT=$(cd "$(dirname "$0")/.." && pwd)
IMAGE=${MOSDNS_IMAGE:-mosdns:p2-tools}
SCRIPTS_DIR=${MOSDNS_P2_SCRIPTS_DIR:-$(cd "$ROOT/scripts" && pwd)}
NET=mosdns-p2-test
SUT=mosdns-p2-sut
WORK="$ROOT/tests/.runtime/run-$$"
EVIDENCE_DIR="$ROOT/.cursor/tmp/p2-integration-$(date '+%Y%m%dT%H%M%S')"
PASS=0
FAIL=0
CREATED_NET=0
CREATED_CN=0
CREATED_GL=0
CREATED_BOGUS=0
CREATED_PRIV=0
CREATED_SUT=0

ok() { PASS=$((PASS + 1)); printf 'PASS T%02d %s\n' "$1" "$2"; }
ng() { FAIL=$((FAIL + 1)); printf 'FAIL T%02d %s\n' "$1" "$2"; }
blocked() { FAIL=$((FAIL + 1)); printf 'BLOCKED T%02d %s\n' "$1" "$2"; }

cleanup() {
  if [ "${MOSDNS_P2_KEEP:-0}" = 1 ]; then
    printf '保留现场：%s、网络 %s\n' "$WORK" "$NET" >&2
    return 0
  fi
  [ "$CREATED_SUT" = 1 ] && docker rm -fv "$SUT" >/dev/null 2>&1 || true
  [ "$CREATED_CN" = 1 ] && docker rm -fv mosdns-p2-cn >/dev/null 2>&1 || true
  [ "$CREATED_GL" = 1 ] && docker rm -fv mosdns-p2-gl >/dev/null 2>&1 || true
  [ "$CREATED_BOGUS" = 1 ] && docker rm -fv mosdns-p2-bogus >/dev/null 2>&1 || true
  [ "$CREATED_PRIV" = 1 ] && docker rm -fv mosdns-p2-priv >/dev/null 2>&1 || true
  [ "$CREATED_NET" = 1 ] && docker network rm "$NET" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

if ! command -v docker >/dev/null 2>&1 || ! command -v dig >/dev/null 2>&1; then
  blocked 1 'runner 缺少 docker CLI 或 dig；需要 mosdns:p2-tools runner'
  exit 1
fi
if docker network inspect "$NET" >/dev/null 2>&1; then
  blocked 2 "$NET 已存在，拒绝触碰已有网络"
  exit 1
fi
for n in "$SUT" mosdns-p2-cn mosdns-p2-gl mosdns-p2-bogus mosdns-p2-priv; do
  if docker inspect "$n" >/dev/null 2>&1; then
    blocked 3 "$n 已存在，拒绝触碰已有容器"
    exit 1
  fi
done
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
  blocked 4 "缺少测试镜像 $IMAGE"
  exit 1
fi

mkdir -p "$WORK/rules" "$WORK/state" "$WORK/host"
mkdir -p "$EVIDENCE_DIR"
cp "$ROOT"/tests/fixtures/* "$WORK/rules/"
chmod -R 777 "$WORK"
# Bind-mounted source files need executable mode even after an editor replaces them.
chmod +x "$SCRIPTS_DIR"/*.sh "$ROOT/tests/mock_upstream.sh"

if docker network create --subnet 172.31.250.0/24 "$NET" >/dev/null 2>&1; then CREATED_NET=1; else blocked 5 '创建测试网络失败'; exit 1; fi

start_upstream() {
  name=$1
  ip=$2
  address=$3
  docker run -d --name "$name" --network "$NET" --entrypoint /opt/tests/mock_upstream.sh \
    --ip "$address" -v "$ROOT/tests:/opt/tests:ro" "$IMAGE" "$ip" 53 >/dev/null 2>&1
}
if start_upstream mosdns-p2-cn 192.0.2.1 172.31.250.2; then CREATED_CN=1; else blocked 6 '国内 mock 上游启动失败'; exit 1; fi
if start_upstream mosdns-p2-gl 198.51.100.1 172.31.250.3; then CREATED_GL=1; else blocked 7 'global mock 上游启动失败'; exit 1; fi
if start_upstream mosdns-p2-bogus 192.0.2.99 172.31.250.4; then CREATED_BOGUS=1; else blocked 8 'bogus mock 上游启动失败'; exit 1; fi
if start_upstream mosdns-p2-priv 172.16.8.50 172.31.250.5; then CREATED_PRIV=1; else blocked 9 'private mock 上游启动失败'; exit 1; fi
CN_IP=172.31.250.2
GL_IP=172.31.250.3

if docker run -d --name "$SUT" --network "$NET" \
  -v "$SCRIPTS_DIR:/opt/mosdns:ro" \
  -v "$ROOT/tests:/opt/tests:ro" \
  -v "$WORK/rules:/etc/mosdns/rules" \
  -v "$WORK/state:/data/state" \
  -v "$WORK/host:/etc/mosdns/host" \
  -e CONTAINER_DNS=223.5.5.5 -e "DNS_CN=$CN_IP" -e "DNS_GLOBAL=$GL_IP" -e CANARY_SENTINEL=pin.test -e REL_FLOOR_PCT=0 -e FALLBACK_THRESHOLD=2000 -e FALLBACK_STANDBY=false \
  -e RULES_SOURCES= -e CUSTOM_HOSTS_SOURCES= -e RULES_UPDATE_INTERVAL=86400 \
  --entrypoint /opt/mosdns/entrypoint.sh "$IMAGE" >/dev/null 2>&1; then
  CREATED_SUT=1
else
  blocked 10 'mosdns 被测实例启动失败'
  exit 1
fi

# Wait for both mock listeners before issuing queries through the SUT.
for upstream in "$CN_IP" "$GL_IP"; do
  expected=192.0.2.1; [ "$upstream" = "$GL_IP" ] && expected=198.51.100.1
  attempt=0
  while [ "$attempt" -lt 20 ]; do
    if docker exec "$SUT" dig @"$upstream" upstream-ready.test +time=1 +tries=1 +short 2>/dev/null | grep -Fxq "$expected"; then break; fi
    sleep 0.05
    attempt=$((attempt + 1))
  done
done
ready=0
i=0
while [ "$i" -lt 40 ]; do
  if docker exec "$SUT" dig @127.0.0.1 -p 53 pin.test +tries=1 +time=1 +short 2>/dev/null | grep -Fxq 192.0.2.10; then ready=1; break; fi
  sleep 0.5
  i=$((i + 1))
done
if [ "$ready" = 1 ]; then ok 11 '被测 DNS 服务就绪'; printf 'PASS T11 被测 DNS 服务就绪\n' > "$EVIDENCE_DIR/readiness.log"; else
  ng 11 '被测 DNS 未就绪'
  docker inspect -f '{{.State.Status}} {{.State.ExitCode}} {{.State.Error}}' "$SUT" >&2
  docker logs "$SUT" >&2
  exit 1
fi

# SUT rewrites /etc/resolv.conf to CONTAINER_DNS, so Docker's embedded DNS
# names are not used for upstreams; pass deterministic network IPs instead.
export MOSDNS_P2_CN_IP="$CN_IP" MOSDNS_P2_GL_IP="$GL_IP"

export ROOT MOSDNS_IMAGE="$IMAGE" MOSDNS_P2_SUT="$SUT" MOSDNS_P2_NET="$NET" MOSDNS_P2_WORK="$WORK" MOSDNS_P2_EVIDENCE="$EVIDENCE_DIR"

suite_no=12
for suite in routing sync runtime scenarios; do
  log_file="$EVIDENCE_DIR/$suite.log"
  rc=0
  if [ ! -f "$ROOT/tests/$suite.sh" ]; then
    blocked "$suite_no" "缺少 tests/$suite.sh"
    suite_no=$((suite_no + 1))
    continue
  fi
  timeout 1200 sh "$ROOT/tests/$suite.sh" > "$log_file" 2>&1 || rc=$?
  cat "$log_file"
  count_pass=$(grep -Ec '^PASS ' "$log_file" || true)
  count_fail=$(grep -Ec '^(FAIL|BLOCKED) ' "$log_file" || true)
  PASS=$((PASS + count_pass))
  FAIL=$((FAIL + count_fail))
  if [ "$rc" != 0 ] && [ "$count_fail" = 0 ]; then
    if [ "$rc" = 124 ] || [ "$rc" = 143 ]; then
      blocked "$suite_no" "$suite 超过20分钟，保留日志并继续下一组"
    else
      ng "$suite_no" "$suite 异常退出 rc=$rc；见 $log_file"
    fi
  fi
  suite_no=$((suite_no + 1))
done
printf '总汇总 PASS=%s FAIL=%s\n' "$PASS" "$FAIL"
printf '证据目录 %s\n' "$EVIDENCE_DIR"
printf '总汇总 PASS=%s FAIL=%s\n' "$PASS" "$FAIL" > "$EVIDENCE_DIR/summary.txt"
exit "$FAIL"

#!/bin/sh
# 离线同步链验收；由 tests/run.sh 在 docker-cli + bind-tools runner 中调用。
set -u

ROOT=${ROOT:-$(cd "$(dirname "$0")/.." && pwd)}
IMAGE=${MOSDNS_IMAGE:-mosdns:p2-tools}
SUT=${MOSDNS_P2_SUT:-mosdns-p2-sut}
NET=${MOSDNS_P2_NET:-mosdns-p2-test}
WORK=${MOSDNS_P2_WORK:?run.sh must set MOSDNS_P2_WORK}
SRC="$WORK/sync-src"
PASS=0
FAIL=0

ok() { PASS=$((PASS + 1)); printf 'PASS S%02d %s\n' "$1" "$2"; }
ng() { FAIL=$((FAIL + 1)); printf 'FAIL S%02d %s\n' "$1" "$2"; }

cleanup() {
  docker rm -fv mosdns-p2-sync-a mosdns-p2-sync-b mosdns-p2-sync-auth >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

mkdir -p "$SRC/a" "$SRC/b"
write_rules() {
  d=$1
  marker=$2
  printf 'baidu.com\n%s.test\n' "$marker" > "$d/cn-domains.txt"
  printf 'soft.test\napple.test\noverflow.test\n' > "$d/proxy-domains.txt"
  printf 'google.com\n' > "$d/gfw-domains.txt"
  printf 'ai.test\n' > "$d/ai-domains.txt"
  printf '119.29.29.29/32\n223.5.5.5/32\n114.114.114.114/32\n183.56.180.152/32\n113.80.87.62/32\n' > "$d/IPv4.China.list"
  printf 'bogus-nxdomain=192.0.2.99\n' > "$d/bogus-nxdomain.china.conf"
  printf 'apps.apple.com\n' > "$d/cn-exceptions.txt"
  printf 'apple.com\n' > "$d/apple-cn.txt"
  printf 'pin.test 192.0.2.10\n' > "$d/custom-hosts.txt"
}

write_rules "$SRC/a" source-a
write_rules "$SRC/b" source-b
printf 'bogus-nxdomain=192.0.2.1\n' > "$SRC/b/bogus-nxdomain.china.conf"
rm -f "$SRC/a/proxy-domains.txt" # A is incomplete; it must not be mixed with B.
docker run -d --name mosdns-p2-sync-a --network "$NET" --entrypoint /bin/busybox-extras \
  --ip 172.31.250.20 -v "$SRC/a:/www:ro" "$IMAGE" httpd -f -p 8080 -h /www >/dev/null 2>&1
docker run -d --name mosdns-p2-sync-b --network "$NET" --entrypoint /bin/busybox-extras \
  --ip 172.31.250.21 -v "$SRC/b:/www:ro" "$IMAGE" httpd -f -p 8080 -h /www >/dev/null 2>&1
sleep 1
SRC_A_IP=172.31.250.20
SRC_B_IP=172.31.250.21

SOURCES_A_B="http://$SRC_A_IP:8080,http://$SRC_B_IP:8080"
mkdir -p "$WORK/state/staging"
if docker exec -e "RULES_SOURCES=$SOURCES_A_B" -e MIN_CN=1 -e MIN_PROXY=1 -e MIN_GFW=1 -e MIN_AI=1 -e MIN_IP=1 -e MIN_BOGUS=0 \
  "$SUT" /opt/mosdns/fetch.sh >"$WORK/fetch-source-switch.log" 2>&1; then
  if grep -q '^source-b.test$' "$WORK/state/staging/cn-domains.txt" && ! grep -q '^source-a.test$' "$WORK/state/staging/cn-domains.txt"; then
    ok 1 '缺文件源整体拒绝，全部文件取自完整源 B'
  else
    ng 1 '源 A/B 被混用或未切到完整源 B'
  fi
  if [ "$(cat "$WORK/state/last_source" 2>/dev/null)" = "http://$SRC_B_IP:8080" ]; then
    ok 2 '成功源写入粘性状态'
  else
    ng 2 '成功源未记录为粘性状态'
  fi
else
  docker inspect -f '{{.State.Status}} {{.State.ExitCode}} {{.State.Error}}' mosdns-p2-sync-a mosdns-p2-sync-b 2>/dev/null || true
  docker logs mosdns-p2-sync-a mosdns-p2-sync-b 2>&1 || true
  cat "$WORK/fetch-source-switch.log"
  ng 1 '完整源 B 下载失败'
  ng 2 '成功源粘性状态未建立'
fi

# Create a minimally valid full staging set that passes the real sentinel checks.
cp "$SRC/b"/* "$WORK/state/staging/"
mkdir -p "$WORK/state/lkg"
cp "$WORK/rules"/* "$WORK/state/lkg/" 2>/dev/null || true
pid_before_first=$(docker exec "$SUT" cat /tmp/mosdns.pid 2>/dev/null)
docker exec -e MIN_CN=1 -e MIN_PROXY=1 -e MIN_GFW=1 -e MIN_AI=1 -e MIN_IP=1 -e MIN_BOGUS=0 \
  "$SUT" /opt/mosdns/apply.sh --now --force >"$WORK/apply-initial.log" 2>&1
apply_rc=$?
pid_after_first=$(docker exec "$SUT" cat /tmp/mosdns.pid 2>/dev/null)
if [ "$apply_rc" = 0 ] && [ -n "$pid_after_first" ] && [ "$pid_after_first" != "$pid_before_first" ] \
  && grep -q '^source-b.test$' "$WORK/rules/cn-domains.txt" \
  && grep -q '^192.0.2.1$' "$WORK/rules/bogus-ip.txt" \
  && docker exec "$SUT" dig @127.0.0.1 -p 53 unknown.test +tries=1 +time=4 +short 2>/dev/null | grep -q '^198.51.100.1$'; then
  ok 3 '候选规则成功应用且新 bogus IP 生效'
else
  cat "$WORK/apply-initial.log"
  ng 3 '候选规则应用失败或新 bogus IP 未生效'
fi

# Same content must leave the main pid unchanged.
rm -rf "$WORK/state/staging"; mkdir -p "$WORK/state/staging"
cp "$WORK/rules"/* "$WORK/state/staging/"
before=$(docker exec "$SUT" cat /tmp/mosdns.pid 2>/dev/null)
docker exec -e MIN_CN=1 -e MIN_PROXY=1 -e MIN_GFW=1 -e MIN_AI=1 -e MIN_IP=1 -e MIN_BOGUS=0 \
  "$SUT" /opt/mosdns/apply.sh --now >"$WORK/apply-nochange.log" 2>&1
after=$(docker exec "$SUT" cat /tmp/mosdns.pid 2>/dev/null)
if [ -n "$before" ] && [ "$before" = "$after" ]; then ok 4 '内容无变化时不重启'; else ng 4 '内容无变化却重启或 PID 状态异常'; fi

# A reversed hosts line must be rejected before any restart.
cp "$WORK/rules"/* "$WORK/state/staging/"
printf '192.0.2.20 pin.test\n' > "$WORK/state/staging/custom-hosts.txt"
before=$(docker exec "$SUT" cat /tmp/mosdns.pid 2>/dev/null)
if docker exec -e MIN_CN=1 -e MIN_PROXY=1 -e MIN_GFW=1 -e MIN_AI=1 -e MIN_IP=1 -e MIN_BOGUS=0 \
  "$SUT" /opt/mosdns/apply.sh --now --force >"$WORK/apply-reversed-hosts.log" 2>&1; then
  ng 5 '写反 custom-hosts 意外通过'
else
  after=$(docker exec "$SUT" cat /tmp/mosdns.pid 2>/dev/null)
  if [ "$before" = "$after" ]; then ok 5 '写反格式拒绝且主实例 PID 不变'; else ng 5 '写反格式拒绝但主实例 PID 变化'; fi
fi

# The validator accepts a syntactically IPv6-shaped but invalid address; mosdns
# must reject it in canary, and the active process must remain untouched.
cp "$WORK/rules"/* "$WORK/state/staging/"
printf 'pin.test ::::\n' > "$WORK/state/staging/custom-hosts.txt"
before=$(docker exec "$SUT" cat /tmp/mosdns.pid 2>/dev/null)
if docker exec -e MIN_CN=1 -e MIN_PROXY=1 -e MIN_GFW=1 -e MIN_AI=1 -e MIN_IP=1 -e MIN_BOGUS=0 \
  "$SUT" /opt/mosdns/apply.sh --now --force >"$WORK/apply-canary-failure.log" 2>&1; then
  ng 6 'mosdns 格式错误候选意外通过金丝雀'
else
  after=$(docker exec "$SUT" cat /tmp/mosdns.pid 2>/dev/null)
  canary_log=$(docker exec "$SUT" cat /data/state/canary/log 2>/dev/null)
  if [ "$before" = "$after" ] && printf '%s' "$canary_log" | grep -Eiq 'invalid|parse|ip|failed|error'; then
    ok 6 'mosdns 明确拒绝候选，金丝雀失败且主实例 PID 不变'
  else
    ng 6 '没有金丝雀拒绝证据或主实例 PID 变化'
  fi
fi

# Fail post-check deliberately on an unused port. The apply path must restore
# previous active files; the regular DNS listener remains independently checked.
cp "$WORK/rules"/* "$WORK/state/staging/"
printf 'pin.test 192.0.2.21\n' > "$WORK/state/staging/custom-hosts.txt"
old_hosts=$(cat "$WORK/rules/custom-hosts.txt" 2>/dev/null)
before=$(docker exec "$SUT" cat /tmp/mosdns.pid 2>/dev/null)
post_out="$WORK/postcheck.log"
if docker exec -e MIN_CN=1 -e MIN_PROXY=1 -e MIN_GFW=1 -e MIN_AI=1 -e MIN_IP=1 -e MIN_BOGUS=0 \
  -e DNS_ENTRY_PORT=19999 -e RESTART_TIMEOUT=1 -e CANARY_PORT=15354 \
  "$SUT" /opt/mosdns/apply.sh --now --force >"$post_out" 2>&1; then
  ng 7 '故意失败的后检意外通过'
else
  new_hosts=$(cat "$WORK/rules/custom-hosts.txt" 2>/dev/null)
  after=$(docker exec "$SUT" cat /tmp/mosdns.pid 2>/dev/null)
  live=$(docker exec "$SUT" dig @127.0.0.1 -p 53 pin.test +tries=1 +time=1 +short 2>/dev/null | head -1)
  if [ "$old_hosts" = "$new_hosts" ] && [ -n "$after" ] && [ "$after" != "$before" ] \
    && [ "$live" = 192.0.2.10 ] && grep -q '后检失败' "$post_out"; then
    ok 7 '后检失败有证据，hosts恢复旧值且新 PID 的真实 DNS 恢复'
  else
    ng 7 '后检失败未能证明规则回滚和真实 DNS 恢复'
  fi
fi

# Fetch exhaustion never touches the last good active rules.
before=$(sha256sum "$WORK/rules/cn-domains.txt" | cut -d' ' -f1)
if docker exec -e "RULES_SOURCES=http://$SRC_A_IP:8080" -e RULES_FETCH_TIMEOUT=1 \
  "$SUT" /opt/mosdns/fetch.sh >"$WORK/fetch-all-fail.log" 2>&1; then
  ng 8 '所有源不可用時 fetch 意外成功'
else
  after=$(sha256sum "$WORK/rules/cn-domains.txt" | cut -d' ' -f1)
  if [ "$before" = "$after" ]; then ok 8 '所有源不可用时保留 LKG/线上规则'; else ng 8 '源失败改变了线上规则'; fi
fi

# A real reload must not accumulate exponential supervisor backoff.
reload_times=0
iteration=1
while [ "$iteration" -le 2 ]; do
  rm -rf "$WORK/state/staging"; mkdir -p "$WORK/state/staging"
  cp "$WORK/rules"/* "$WORK/state/staging/"
  printf 'pin.test 192.0.2.%s\n' "$((30 + iteration))" > "$WORK/state/staging/custom-hosts.txt"
  t0=$(awk '{ split($1,a,"."); print a[1]*1000+substr(a[2]"000",1,3) }' /proc/uptime)
  docker exec -e MIN_CN=1 -e MIN_PROXY=1 -e MIN_GFW=1 -e MIN_AI=1 -e MIN_IP=1 -e MIN_BOGUS=0 \
    "$SUT" /opt/mosdns/apply.sh --now --force >"$WORK/apply-reload-$iteration.log" 2>&1 || reload_times=$((reload_times + 1))
  ready=0
  i=0
  while [ "$i" -lt 20 ]; do
    if docker exec "$SUT" dig @127.0.0.1 -p 53 pin.test +tries=1 +time=1 +short 2>/dev/null | grep -q .; then ready=1; break; fi
    sleep 0.25; i=$((i + 1))
  done
  t1=$(awk '{ split($1,a,"."); print a[1]*1000+substr(a[2]"000",1,3) }' /proc/uptime)
  [ "$ready" = 1 ] || reload_times=$((reload_times + 1))
  [ $((t1 - t0)) -le 5000 ] || reload_times=$((reload_times + 1))
  iteration=$((iteration + 1))
done
if [ "$reload_times" = 0 ]; then ok 9 '连续两次有效规则重载均无累积退避'; else ng 9 '连续重载失败或耗时超过 5 秒'; fi

. "$ROOT/tests/sync-extra.sh"

printf '同步链汇总 PASS=%s FAIL=%s\n' "$PASS" "$FAIL"
if [ -n "${MOSDNS_P2_EVIDENCE:-}" ]; then cp "$WORK"/*.log "$MOSDNS_P2_EVIDENCE/" 2>/dev/null || true; fi
exit "$FAIL"

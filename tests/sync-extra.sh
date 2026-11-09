#!/bin/sh
# Sourced by sync.sh; shares its numbered checks, paths and cleanup.
apply_test() {
  docker exec -e MIN_CN=1 -e MIN_PROXY=1 -e MIN_GFW=1 -e MIN_AI=1 -e MIN_IP=1 -e MIN_BOGUS=0 \
    "$SUT" /opt/mosdns/apply.sh --now "$@"
}
fetch_test() {
  docker exec -e MIN_CN=1 -e MIN_PROXY=1 -e MIN_GFW=1 -e MIN_AI=1 -e MIN_IP=1 -e MIN_BOGUS=0 \
    -e "RULES_SOURCES=$1" "$SUT" /opt/mosdns/fetch.sh
}
sync_hosts_test() {
  docker exec -e MIN_CN=1 -e MIN_PROXY=1 -e MIN_GFW=1 -e MIN_AI=1 -e MIN_IP=1 -e MIN_BOGUS=0 \
    -e RULES_FETCH_TIMEOUT=1 -e "CUSTOM_HOSTS_SOURCES=$1" \
    "$SUT" /opt/mosdns/sync-loop.sh --custom-hosts-once
}
pin_answer() {
  docker exec "$SUT" dig @127.0.0.1 pin.test +time=1 +tries=1 +short 2>/dev/null | head -1
}

# HTTP basic-auth challenge proves that an Authorization header was received.
# These credentials are synthetic test data, created only under the runtime directory.
cp "$WORK/rules/custom-hosts.txt" "$SRC/b/custom-hosts.txt"
printf '/custom-hosts.txt:p2test:p2test\n' > "$SRC/b/httpd.conf"
docker run -d --name mosdns-p2-sync-auth --network "$NET" --ip 172.31.250.22 \
  --entrypoint /bin/busybox-extras -v "$SRC/b:/www:ro" "$IMAGE" \
  httpd -f -p 8080 -h /www -c /www/httpd.conf >/dev/null 2>&1
sleep 0.2
auth_url=http://172.31.250.22:8080/custom-hosts.txt
secret="Basic $(printf 'p2test:p2test' | base64 | tr -d '\n')"
unauth_rc=0
docker exec "$SUT" wget -q -T 1 -O /dev/null "$auth_url" >/dev/null 2>&1 || unauth_rc=$?
auth_rc=0
docker exec -e "CUSTOM_HOSTS_SOURCES=$auth_url" -e "CUSTOM_HOSTS_AUTH=$secret" \
  "$SUT" /opt/mosdns/sync-loop.sh --custom-hosts-once > "$WORK/custom-host-auth.log" 2>&1 || auth_rc=$?
docker logs "$SUT" > "$WORK/auth-logs.txt" 2>&1 || true
docker logs mosdns-p2-sync-auth >> "$WORK/auth-logs.txt" 2>&1 || true
if [ "$unauth_rc" -ne 0 ] && [ "$auth_rc" = 0 ] \
  && ! grep -Fq "$secret" "$WORK/state/sync.log" \
  && ! grep -Fq "$secret" "$WORK/auth-logs.txt" \
  && ! grep -Fq "$secret" "$WORK/custom-host-auth.log"; then
  ok 10 '无认证请求被拒、携带AUTH请求成功，日志不泄漏凭据'
else
  ng 10 "AUTH挑战或日志检查失败 unauth_rc=$unauth_rc auth_rc=$auth_rc"
fi
docker rm -fv mosdns-p2-sync-auth >/dev/null 2>&1 || true

# A complete source with invalid format must be rejected before selecting B.
cp "$SRC/b"/*.txt "$SRC/a/"
cp "$SRC/b"/*.list "$SRC/a/"
cp "$SRC/b"/*.conf "$SRC/a/"
printf 'baidu.com\ndomain:invalid.test\n' > "$SRC/a/cn-domains.txt"
rm -f "$WORK/state/last_source"
if fetch_test "$SOURCES_A_B" > "$WORK/fetch-invalid-source.log" 2>&1 \
  && [ "$(cat "$WORK/state/last_source")" = "http://$SRC_B_IP:8080" ] \
  && grep -q '校验失败' "$WORK/fetch-invalid-source.log"; then
  ok 11 '完整但格式错误的源A被拒，整套切换到B'
else
  ng 11 '错误源未拒绝或未切换到完整有效源'
fi

# A removed sticky source must never override the current configured source list.
: > "$SRC/a/cn-domains.txt"
old_hash=$(sha256sum "$WORK/rules/cn-domains.txt" | cut -d' ' -f1)
old_pid=$(docker exec "$SUT" cat /tmp/mosdns.pid)
rc=0
fetch_test "http://$SRC_A_IP:8080" > "$WORK/fetch-empty-source.log" 2>&1 || rc=$?
if [ "$rc" -ne 0 ] \
  && [ "$old_hash" = "$(sha256sum "$WORK/rules/cn-domains.txt" | cut -d' ' -f1)" ] \
  && [ "$old_pid" = "$(docker exec "$SUT" cat /tmp/mosdns.pid)" ]; then
  ok 12 '所有配置源被清空时拒绝，保持线上规则及主PID'
else
  ng 12 '清空源被接受，或移除的粘性源仍被使用'
fi

# custom-hosts is intentionally a single-primary contract.
old_hosts=$(cat "$WORK/rules/custom-hosts.txt")
printf 'pin.test 192.0.2.42\n' > "$SRC/b/custom-hosts.txt"
old_pid=$(docker exec "$SUT" cat /tmp/mosdns.pid)
rc=0
sync_hosts_test "http://$SRC_A_IP:9/custom-hosts.txt,http://$SRC_B_IP:8080/custom-hosts.txt" \
  > "$WORK/hosts-primary-down.log" 2>&1 || rc=$?
if [ "$rc" -ne 0 ] && [ "$old_hosts" = "$(cat "$WORK/rules/custom-hosts.txt")" ] \
  && [ "$old_pid" = "$(docker exec "$SUT" cat /tmp/mosdns.pid)" ]; then
  ok 13 'hosts主源不可达时保持当前版本，不切第二URL'
else
  ng 13 'hosts主源失败时发生非预期切换'
fi

rc=0
sync_hosts_test "http://$SRC_B_IP:8080/custom-hosts.txt" > "$WORK/hosts-update.log" 2>&1 || rc=$?
updated=$(pin_answer)
printf '%s\n' "$old_hosts" > "$SRC/b/custom-hosts.txt"
sync_hosts_test "http://$SRC_B_IP:8080/custom-hosts.txt" > "$WORK/hosts-revert.log" 2>&1 || rc=$?
restored=$(pin_answer)
old_pin=$(printf '%s\n' "$old_hosts" | awk '$1=="pin.test" {print $2; exit}')
if [ "$rc" = 0 ] && [ "$updated" = 192.0.2.42 ] && [ "$restored" = "$old_pin" ]; then
  ok 14 '合法hosts更新及恢复旧文件均通过同步生效'
else
  ng 14 "hosts更新/恢复失败 rc=$rc updated=$updated restored=$restored"
fi

# Real repeated fetch/apply, not a copy of live followed by a hash comparison.
rc=0
fetch_test "http://$SRC_B_IP:8080" > "$WORK/fetch-unchanged-1.log" 2>&1 || rc=$?
apply_test > "$WORK/apply-unchanged-1.log" 2>&1 || rc=$?
old_pid=$(docker exec "$SUT" cat /tmp/mosdns.pid)
fetch_test "http://$SRC_B_IP:8080" > "$WORK/fetch-unchanged-2.log" 2>&1 || rc=$?
apply_test > "$WORK/apply-unchanged-2.log" 2>&1 || rc=$?
if [ "$rc" = 0 ] && [ "$old_pid" = "$(docker exec "$SUT" cat /tmp/mosdns.pid)" ] \
  && grep -q '无变化' "$WORK/apply-unchanged-2.log" \
  && ! grep -q '校验规则目录' "$WORK/fetch-unchanged-2.log"; then
  ok 15 '重复同源fetch/apply不重新校验、不重启'
else
  ng 15 '真实同步无变化时仍校验、重启或流程失败'
fi

old_loop=$(docker exec "$SUT" pgrep -f '^/bin/sh /opt/mosdns/sync-loop.sh$' | head -1)
if [ -n "$old_loop" ]; then
  docker exec "$SUT" kill -TERM "$old_loop"
  i=0; dns_fail=0; new_loop=
  while [ "$i" -lt 76 ]; do
    [ "$(pin_answer)" = "$old_pin" ] || dns_fail=$((dns_fail + 1))
    new_loop=$(docker exec "$SUT" pgrep -f '^/bin/sh /opt/mosdns/sync-loop.sh$' | head -1)
    [ -n "$new_loop" ] && [ "$new_loop" != "$old_loop" ] && break
    sleep 0.5
    i=$((i + 1))
  done
  if [ "$dns_fail" = 0 ] && [ -n "$new_loop" ] && [ "$new_loop" != "$old_loop" ]; then
    ok 16 '同步循环退出后DNS持续可用，监督器重新拉起循环'
  else
    ng 16 "同步循环监督检查失败 dns_fail=$dns_fail"
  fi
else
  ng 16 '未找到实际同步循环进程'
fi

rc=0
docker exec -e MIN_CN=1 -e MIN_PROXY=1 -e MIN_GFW=1 -e MIN_AI=1 -e MIN_IP=1 -e MIN_BOGUS=0 \
  -e RULES_SOURCES=http://gh-proxy.com:8080 -e "FETCH_RESOLVE=gh-proxy.com:8080:$SRC_B_IP" \
  "$SUT" /opt/mosdns/fetch.sh > "$WORK/fetch-mirror.log" 2>&1 || rc=$?
if [ "$rc" = 0 ] && grep -Eiq 'WARN.*mirror.*unverified' "$WORK/fetch-mirror.log"; then
  ok 17 '仅第三方镜像可达时接受并明确告警尚未验证完整性'
else
  ng 17 '镜像降级接受未成功或未告警'
fi

# Establish a real 20-line healthy LKG, then reject a 14-line source (<80%).
{
  printf 'baidu.com\n'
  n=1
  while [ "$n" -lt 20 ]; do printf 'full-%s.test\n' "$n"; n=$((n + 1)); done
} > "$SRC/b/cn-domains.txt"
rc=0
fetch_test "http://$SRC_B_IP:8080" > "$WORK/fetch-grow.log" 2>&1 || rc=$?
apply_test > "$WORK/apply-grow.log" 2>&1 || rc=$?
cp "$SRC/b"/*.txt "$SRC/a/"
cp "$SRC/b"/*.list "$SRC/a/"
cp "$SRC/b"/*.conf "$SRC/a/"
head -14 "$SRC/b/cn-domains.txt" > "$SRC/a/cn-domains.txt"
rm -f "$WORK/state/last_source"
docker exec -e MIN_CN=1 -e MIN_PROXY=1 -e MIN_GFW=1 -e MIN_AI=1 -e MIN_IP=1 -e MIN_BOGUS=0 \
  -e REL_FLOOR_PCT=80 -e "RULES_SOURCES=$SOURCES_A_B" \
  "$SUT" /opt/mosdns/fetch.sh > "$WORK/fetch-relative-floor.log" 2>&1 || rc=$?
if [ "$rc" = 0 ] && [ "$(cat "$WORK/state/last_source")" = "http://$SRC_B_IP:8080" ] \
  && grep -q '80%' "$WORK/fetch-relative-floor.log"; then
  ok 18 '真实LKG的规则缩水超过20%时拒绝A并切换完整B'
else
  ng 18 '相对LKG缩水保护或后续源切换失败'
fi

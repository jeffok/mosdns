#!/bin/sh
# Run inside the test runner after mosdns-p2-sut is ready.
set -u

SUT=${MOSDNS_P2_SUT:-mosdns-p2-sut}
PASS=0
FAIL=0

query() {
  out=$(docker exec "$SUT" dig @127.0.0.1 -p 53 "$1" "${2:-A}" +time=2 +tries=1 +noall +comments +answer 2>&1)
  rc=$?
  [ "${MOSDNS_P2_DEBUG:-0}" = 1 ] && { printf 'QUERY %s %s rc=%s\n%s\n' "$1" "${2:-A}" "$rc" "$out" >&2; }
  printf '%s\n' "$out"
  return 0
}

rcode() { query "$1" "${2:-A}" | sed -n 's/.*status: \([^,]*\),.*/\1/p' | head -n 1; }
answers() { query "$1" "${2:-A}" | awk '!/^;/ && /[[:space:]]IN[[:space:]]/ { n++ } END { print n+0 }'; }
addresses() { query "$1" "${2:-A}" | awk '!/^;/ && /[[:space:]]IN[[:space:]]A[[:space:]]/ { print $NF }'; }

check() {
  id=$1; description=$2; expected=$3; actual=$4
  if [ "$actual" = "$expected" ]; then
    PASS=$((PASS+1)); printf 'PASS %s %s\n' "$id" "$description"
  else
    FAIL=$((FAIL+1)); printf 'FAIL %s %s (expected=%s actual=%s)\n' "$id" "$description" "$expected" "$actual"
  fi
}

check R01 'AAAA rcode is NOERROR' NOERROR "$(rcode www.cn.test AAAA)"
check R02 'AAAA has no answers' 0 "$(answers www.cn.test AAAA)"
check R03 'custom-host exact A response' 192.0.2.10 "$(addresses pin.test A)"
check R04 'custom-host domain prefix includes child' 192.0.2.11 "$(addresses a.sub.pin.test A)"
check R16 'exact pin does not match a sibling subdomain' 192.0.2.1 "$(addresses a.pin.test A)"
check R05 'custom-host AAAA is NODATA and stops routing' NOERROR "$(rcode pin.test AAAA)"
check R06 'custom-host NODATA contains no answers' 0 "$(answers pin.test AAAA)"
pin_cn_count=$(docker logs mosdns-p2-cn 2>&1 | grep -F '"qname": "pin.test."' | wc -l | tr -d ' ')
pin_gl_count=$(docker logs mosdns-p2-gl 2>&1 | grep -F '"qname": "pin.test."' | wc -l | tr -d ' ')
pin_upstream_count=$((pin_cn_count + pin_gl_count))
check R13 'custom-host NODATA is not sent upstream' 0 "$pin_upstream_count"
cache_first=$(addresses cache.test A)
cache_second=$(addresses cache.test A)
check R14 'first CN cacheable response' 192.0.2.1 "$cache_first"
check R15 'cached CN response survives later pipeline' 192.0.2.1 "$cache_second"
cn_cache_count=$(docker logs mosdns-p2-cn 2>&1 | grep -F 'cache.test' | wc -l | tr -d ' ')
check R17 'cache hit does not call CN upstream twice' 1 "$cn_cache_count"
hard_answer=$(addresses hard.test A)
check R07 'hard-list answer is not replaced by cn fallback' 198.51.100.1 "$hard_answer"
cn_hard_count=$(docker logs mosdns-p2-cn 2>&1 | grep -F 'hard.test' | grep -v 'fonts.hard.test' | wc -l | tr -d ' ')
gl_hard_count=$(docker logs mosdns-p2-gl 2>&1 | grep -F 'hard.test' | grep -v 'fonts.hard.test' | wc -l | tr -d ' ')
check R08 'hard-list does not query cn upstream' 0 "$cn_hard_count"
check R09 'hard-list queries global upstream' 1 "$( [ "$gl_hard_count" -ge 1 ] && printf 1 || printf 0 )"
check R10 'cn exception ahead of hard-list' 192.0.2.1 "$(addresses fonts.hard.test A)"
check R11 'soft-list goto follows cn-first policy' 192.0.2.1 "$(addresses soft.test A)"
check R12 'known CN domain uses CN upstream' 192.0.2.1 "$(addresses www.cn.test A)"

printf 'ROUTING PASS=%s FAIL=%s\n' "$PASS" "$FAIL"
if [ "$FAIL" -eq 0 ]; then exit 0; else exit "$FAIL"; fi

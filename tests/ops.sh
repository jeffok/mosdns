#!/bin/sh
# status.sh / explain.sh 运维脚本验收；由 tests/run.sh 在 runner 中调用。
set -u

ROOT=${ROOT:-$(cd "$(dirname "$0")/.." && pwd)}
IMAGE=${MOSDNS_IMAGE:-mosdns:p2-tools}
PASS=0
FAIL=0
NAME=""

ok() { PASS=$((PASS + 1)); printf 'PASS %s %s\n' "$1" "$2"; }
ng() { FAIL=$((FAIL + 1)); printf 'FAIL %s %s\n' "$1" "$2"; }

work=${MOSDNS_P2_WORK:-${TMPDIR:-/tmp}}/ops-$$
mkdir -p "$work"
cleanup() {
  [ -n "$NAME" ] && docker rm -fv "$NAME" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT INT TERM

rules=$work/rules
state=$work/state
mkdir -p "$rules" "$state"
: > "$rules/apple-cn.txt"
: > "$rules/cn-exceptions.txt"
printf '# comment only\n\n' > "$rules/gfw-domains.txt"
printf '# c\ncn.test\nwww.cn.test\n' > "$rules/cn-domains.txt"
printf 'soft.test\n' > "$rules/proxy-domains.txt"
printf 'ai.test\n' > "$rules/ai-domains.txt"
printf '1.0.1.0/24\n' > "$rules/IPv4.China.list"
printf 'pin.test 192.0.2.10\n' > "$rules/custom-hosts.txt"
# bogus-ip.txt 故意不创建，覆盖缺失文件路径

out=$(RULES_DIR="$rules" STATE_DIR="$state" LKG_DIR="$state/missing-lkg" \
  sh "$ROOT/scripts/status.sh" 2>/dev/null) || true

section=$(printf '%s\n' "$out" | sed -n '/== 当前规则/,/== LKG/p')
extras=$(printf '%s\n' "$section" | grep -cE '^[[:space:]]*0[[:space:]]*$' || true)
if [ "$extras" = 0 ]; then
  ok ST01 '空规则文件不额外打印孤立的 0'
else
  ng ST01 "空文件多打印了孤立 0（count=$extras）"
fi

apple=$(printf '%s\n' "$section" | awk '$1=="apple-cn.txt"{print $2; exit}')
missing=$(printf '%s\n' "$section" | awk '$1=="bogus-ip.txt"{print $2; exit}')
if [ "$apple" = 0 ] && [ "$missing" = 0 ]; then
  ok ST02 '空文件与缺失文件各计为单个 0'
else
  ng ST02 "空/缺失计数不符 apple=$apple bogus=$missing"
fi

cn=$(printf '%s\n' "$section" | awk '$1=="cn-domains.txt"{print $2; exit}')
if [ "$cn" = 2 ]; then
  ok ST03 '有效行计数忽略注释与空行'
else
  ng ST03 "cn-domains 期望 2 实际 $cn"
fi

# --- explain.sh ---
ex_env() {
  RULES_DIR="$rules" EXPLAIN_DNS="${1:-127.0.0.1}" DNS_ENTRY_PORT="${2:-53}" \
    sh "$ROOT/scripts/explain.sh" "$3" "${4:-A}"
}

ex=$(ex_env 127.0.0.1 53 pin.test A 2>/dev/null || true)
if printf '%s\n' "$ex" | grep -q '命中: custom_hosts'; then
  ok EX01 '规则命中不依赖查询端口'
else
  ng EX01 "未识别 custom_hosts：$(printf '%s' "$ex" | tr '\n' ' ')"
fi

nodig=$work/nodig
mkdir -p "$nodig"
if [ ! -x /bin/busybox ]; then
  ng EX02 '需要 busybox 以模拟无 dig 环境'
else
  for a in awk sed tr date head nslookup grep; do ln -sf /bin/busybox "$nodig/$a"; done
  hidden=$(PATH="$nodig:/bin" command -v dig 2>/dev/null || true)
  if [ -n "$hidden" ]; then
    ng EX02 "未能隐藏 dig（仍为 $hidden）"
  else
    skip=$(PATH="$nodig:/bin" RULES_DIR="$rules" EXPLAIN_DNS=127.0.0.1 DNS_ENTRY_PORT=5353 \
      sh "$ROOT/scripts/explain.sh" pin.test A 2>/dev/null || true)
    if printf '%s\n' "$skip" | grep -q 'DNS_ENTRY_PORT=5353' \
      && printf '%s\n' "$skip" | grep -q '没有 dig' \
      && printf '%s\n' "$skip" | grep -q '不支持指定端口'; then
      ok EX02 '非 53 端口且无 dig 时给出明确提示'
    else
      ng EX02 "提示不符：$(printf '%s' "$skip" | tr '\n' ' ')"
    fi
  fi
fi

if ! command -v docker >/dev/null 2>&1; then
  ng EX03 'runner 缺少 docker CLI，无法验收 dig -p'
elif ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
  ng EX03 "缺少测试镜像 $IMAGE"
else
  NAME="mosdns-misc-explain-$$"
  if docker run -d --name "$NAME" \
    --entrypoint /opt/tests/mock_upstream.sh \
    -v "$ROOT/scripts:/opt/mosdns:ro" \
    -v "$ROOT/tests:/opt/tests:ro" \
    -v "$rules:/etc/mosdns/rules:ro" \
    "$IMAGE" 192.0.2.77 5353 >/dev/null 2>&1; then
    ready=0
    i=0
    while [ "$i" -lt 25 ]; do
      if docker exec "$NAME" dig @127.0.0.1 -p 5353 +time=1 +tries=1 +short pin.test A 2>/dev/null | grep -Fq 192.0.2.77; then
        ready=1
        break
      fi
      sleep 0.2
      i=$((i + 1))
    done
    if [ "$ready" = 1 ]; then
      got=$(docker exec -e RULES_DIR=/etc/mosdns/rules -e EXPLAIN_DNS=127.0.0.1 -e DNS_ENTRY_PORT=5353 \
        "$NAME" /opt/mosdns/explain.sh pin.test A 2>/dev/null || true)
      if printf '%s\n' "$got" | grep -q '命中: custom_hosts' \
        && printf '%s\n' "$got" | grep -Fq '192.0.2.77' \
        && ! printf '%s\n' "$got" | grep -q '跳过查询'; then
        ok EX03 '容器内 dig -p 查询 DNS_ENTRY_PORT=5353 得到 mock 应答'
      else
        ng EX03 "dig -p 应答不符：$(printf '%s' "$got" | tr '\n' ' ')"
      fi
    else
      ng EX03 'mosdns-misc-explain mock 未在 5353 就绪'
    fi
  else
    ng EX03 '未能启动 mosdns-misc-explain mock'
  fi
fi

printf 'OPS PASS=%s FAIL=%s\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]

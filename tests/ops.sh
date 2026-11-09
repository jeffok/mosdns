#!/bin/sh
# status.sh / explain.sh 运维脚本验收；由 tests/run.sh 在 runner 中调用。
set -u

ROOT=${ROOT:-$(cd "$(dirname "$0")/.." && pwd)}
PASS=0
FAIL=0

ok() { PASS=$((PASS + 1)); printf 'PASS %s %s\n' "$1" "$2"; }
ng() { FAIL=$((FAIL + 1)); printf 'FAIL %s %s\n' "$1" "$2"; }

work=${MOSDNS_P2_WORK:-${TMPDIR:-/tmp}}/ops-$$
mkdir -p "$work"
cleanup() { rm -rf "$work"; }
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

printf 'OPS PASS=%s FAIL=%s\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]

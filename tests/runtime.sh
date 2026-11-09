#!/bin/sh
# Runtime helper tests for local 04:30 scheduling and validated LKG snapshots.
set -u
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
. "$ROOT/scripts/runtime.sh"

PASS=0
FAIL=0
ok() { PASS=$((PASS + 1)); printf 'PASS %s %s\n' "$1" "$2"; }
ng() { FAIL=$((FAIL + 1)); printf 'FAIL %s %s\n' "$1" "$2" >&2; }
assert_eq() {
  if [ "$1" = "$2" ]; then ok "$3" "$4"; else ng "$3" "$4 (expected '$2', got '$1')"; fi
}

TZ=Asia/Shanghai; export TZ
now=$(date -d '2026-11-09 04:29:00' +%s 2>/dev/null || echo invalid)
next=$(runtime_next_rules "$now" 2>/dev/null || echo invalid)
actual=$(TZ=Asia/Shanghai date -d "@$next" '+%Y-%m-%d %H:%M' 2>/dev/null || echo invalid)
assert_eq "$actual" '2026-11-09 04:30' RT01 'before local 04:30'

now=$(date -d '2026-11-09 04:30:00' +%s 2>/dev/null || echo invalid)
next=$(runtime_next_rules "$now" 2>/dev/null || echo invalid)
actual=$(TZ=Asia/Shanghai date -d "@$next" '+%Y-%m-%d %H:%M' 2>/dev/null || echo invalid)
assert_eq "$actual" '2026-11-10 04:30' RT02 'at local 04:30'

TZ=America/New_York; export TZ
now=$(date -d '2026-03-08 01:30:00' +%s 2>/dev/null || echo invalid)
next=$(runtime_next_rules "$now" 2>/dev/null || echo invalid)
actual=$(TZ=America/New_York date -d "@$next" '+%Y-%m-%d %H:%M' 2>/dev/null || echo invalid)
assert_eq "$actual" '2026-03-08 04:30' RT03 'DST transition day'
TZ=Asia/Shanghai; export TZ

work=${TMPDIR:-/tmp}/mosdns-runtime-test.$$
trap 'rm -rf "$work"' EXIT INT TERM
live=$work/live; lkg=$work/lkg
mkdir -p "$live" "$lkg"
make_valid_rules() {
  dir=$1; mkdir -p "$dir"
  printf '%s\n' 'baidu.com' > "$dir/cn-domains.txt"
  printf '%s\n' '1.0.1.0/24' > "$dir/IPv4.China.list"
  printf '%s\n' 'google.com' > "$dir/gfw-domains.txt"
  printf '%s\n' 'youtube.com' > "$dir/proxy-domains.txt"
  printf '%s\n' 'chatgpt.com' > "$dir/ai-domains.txt"
  : > "$dir/apple-cn.txt"
  : > "$dir/cn-exceptions.txt"
  : > "$dir/bogus-nxdomain.china.conf"
  : > "$dir/bogus-ip.txt"
  : > "$dir/accept-extra.txt"
  : > "$dir/custom-hosts.txt"
  : > "$dir/custom-local.txt"
}
validator() {
  [ -s "$1/cn-domains.txt" ] && [ -s "$1/IPv4.China.list" ] \
    && [ -s "$1/gfw-domains.txt" ] && [ -s "$1/proxy-domains.txt" ]
}
make_valid_rules "$live"

# An absent LKG is rejected without touching live; valid live seeds a complete snapshot.
rm -rf "$lkg"
restore_ok=0
runtime_restore_lkg "$lkg" "$live" validator >/dev/null 2>&1 && restore_ok=1
if [ "$restore_ok" = 0 ] && [ -s "$live/cn-domains.txt" ] \
    && runtime_init_lkg_if_missing "$live" "$lkg" validator; then
  complete=1
  for f in $RUNTIME_MANAGED_RULES; do [ -f "$lkg/$f" ] || complete=0; done
  if [ "$complete" = 1 ]; then ok RT04 'missing LKG rejected; complete live initialized it'; else ng RT04 'initialized LKG is incomplete'; fi
else
  ng RT04 'missing LKG changed live or valid live did not seed LKG'
fi

# Restore validates the complete LKG before updating managed files at startup.
restore_ok=0
if [ -d "$lkg" ] && runtime_restore_lkg "$lkg" "$live" validator; then restore_ok=1; fi
if [ "$restore_ok" = 1 ]; then
  printf '%s\n' 'new-cn.example' > "$lkg/cn-domains.txt"
  if runtime_restore_lkg "$lkg" "$live" validator \
      && grep -qx 'new-cn.example' "$live/cn-domains.txt"; then
    # Verify the production-script invocation passes an empty LKG_DIR.
    external_validator="$work/validator.sh"
    cat > "$external_validator" <<'EOFVALIDATOR'
#!/bin/sh
[ "$1" = --dir ] && [ -d "$2" ] && [ -d "$LKG_DIR" ] || exit 1
[ -z "$(ls -A "$LKG_DIR")" ] || exit 1
[ -s "$2/cn-domains.txt" ] && [ -s "$2/IPv4.China.list" ] \
  && [ -s "$2/gfw-domains.txt" ] && [ -s "$2/proxy-domains.txt" ]
EOFVALIDATOR
    chmod +x "$external_validator"
    if runtime_restore_lkg "$lkg" "$live" "$external_validator"; then
      ok RT05 'valid LKG restored after external --dir validation'
    else
      ng RT05 'external validator invocation failed'
    fi
  else
    ng RT05 'valid LKG was not restored'
  fi
else
  ng RT05 'initialized LKG could not be restored'
fi

# An invalid LKG must be rejected without changing the live tree.
cp -R "$live" "$work/live-before"
: > "$lkg/cn-domains.txt"
restore_ok=0
runtime_restore_lkg "$lkg" "$live" validator >/dev/null 2>&1 && restore_ok=1
if [ "$restore_ok" = 0 ] && diff -r "$work/live-before" "$live" >/dev/null 2>&1; then
  ok RT06 'invalid LKG rejected and live preserved'
else
  ng RT06 'invalid LKG accepted or changed live'
fi

# Promotion publishes all managed files only after validation; invalid live leaves LKG intact.
make_valid_rules "$live"
printf '%s\n' 'fresh.example' > "$live/cn-domains.txt"
if runtime_promote_lkg "$live" "$lkg" validator \
    && grep -qx 'fresh.example' "$lkg/cn-domains.txt"; then
  mkdir -p "$work/lkg-before"
  cp -R "$lkg"/* "$work/lkg-before"/
  : > "$live/cn-domains.txt"
  promote_ok=0
  runtime_promote_lkg "$live" "$lkg" validator >/dev/null 2>&1 && promote_ok=1
  if [ "$promote_ok" = 0 ] && diff -r "$work/lkg-before" "$lkg" >/dev/null 2>&1; then
    ok RT07 'complete snapshot promoted; invalid live left LKG unchanged'
  else
    ng RT07 'invalid live changed LKG'
  fi
else
  ng RT07 'valid live snapshot did not promote'
fi

printf 'SUMMARY PASS=%s FAIL=%s\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]

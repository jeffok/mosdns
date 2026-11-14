#!/bin/sh
# Real mosdns plugin + entry handler + socket chain, with a delayed mock agent.
# The IMAGE must have the entry-timeout patch and contain dig (test-only image).
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
IMAGE=${MOSDNS_IMAGE:-mosdns:guard-timeout-test}
P=mosdns-timeout-test-$$
W=$(mktemp -d)
cleanup() { docker rm -f "$P" >/dev/null 2>&1 || :; rm -rf "$W"; }
trap cleanup EXIT INT TERM
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o "$W/mock" "$ROOT/tests/guard-timeout-mock.go"
printf '1.0.1.0/24\n' > "$W/cn.txt"
cat > "$W/config.yaml" <<'CONFIG'
log:
  level: warn
plugins:
- tag: fixed
  type: hosts
  args:
    entries:
    - "domain:test 8.8.4.4"
- tag: tag_global
  type: ros_bgp
  args:
    mode: tag
- tag: confirm
  type: ros_bgp
  args:
    mode: confirm
    endpoint: http://127.0.0.1:18080/confirm
    guard_writer: true
    sync_budget: 8s
    cn_ip_file: /test/cn.txt
- tag: main
  type: sequence
  args:
  - exec: $fixed
  - exec: $tag_global
  - exec: $confirm
- tag: udp
  type: udp_server
  args:
    entry: main
    listen: 127.0.0.1:15353
- tag: tcp
  type: tcp_server
  args:
    entry: main
    listen: 127.0.0.1:15353
    idle_timeout: 30
CONFIG
docker run -d --name "$P" -e MOSDNS_ENTRY_TIMEOUT=20s -v "$W:/test" --entrypoint sh "$IMAGE" -c '/test/mock & exec mosdns start -c /test/config.yaml' >/dev/null
ready=0
for i in $(seq 1 20); do
 if docker exec "$P" sh -c 'kill -0 1 && netstat -ln 2>/dev/null | grep -q 15353'; then ready=1; break; fi
 sleep 1
done
[ "$ready" = 1 ] || { docker logs "$P" >&2; exit 1; }
for mode in udp tcp; do
 if [ "$mode" = tcp ]; then tcp=+tcp; else tcp=+notcp; fi
 result=$(docker exec "$P" dig @127.0.0.1 -p 15353 delayed.test A "$tcp" +time=12 +tries=1 +short)
 [ "$result" = 8.8.4.4 ] || { echo "$mode delayed DNS failed: $result" >&2; exit 1; }
done
[ "$(wc -l < "$W/finish" | tr -d ' ')" = 2 ] || { echo 'missing finish callbacks' >&2; exit 1; }
echo 'PASS actual UDP/TCP DNS with six-second guard confirmation and send callbacks'

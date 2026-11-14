#!/bin/sh
# Actual native mosdns server: verify the default 5-second deadline and an
# explicitly extended deadline over UDP and TCP without changing production.
set -eu
BIN=${MOSDNS_ENTRY_TEST_BIN:?set MOSDNS_ENTRY_TEST_BIN to the patched native mosdns}
D=$(mktemp -d); PID=
cleanup(){ [ -z "$PID" ] || kill "$PID" 2>/dev/null || :; [ -z "$PID" ] || wait "$PID" 2>/dev/null || :; rm -rf "$D"; }
trap cleanup EXIT INT TERM
PORT=$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')
cat > "$D/config.yaml" <<YAML
log:
  level: error
plugins:
- tag: entry
  type: sequence
  args:
  - exec: sleep 5500
- tag: udp
  type: udp_server
  args:
    entry: entry
    listen: 127.0.0.1:$PORT
- tag: tcp
  type: tcp_server
  args:
    entry: entry
    listen: 127.0.0.1:$PORT
    idle_timeout: 15
YAML
start(){ if [ "$1" = default ]; then env -u MOSDNS_ENTRY_TIMEOUT "$BIN" start -c "$D/config.yaml" > "$D/server.log" 2>&1 & else MOSDNS_ENTRY_TIMEOUT="$1" "$BIN" start -c "$D/config.yaml" > "$D/server.log" 2>&1 & fi; PID=$!; sleep 1; kill -0 "$PID"; }
stop(){ kill "$PID"; wait "$PID" 2>/dev/null || :; PID=; }
start default
dig @127.0.0.1 -p "$PORT" example.com A +time=10 +tries=1 | grep -q 'status: SERVFAIL'
stop
start 8s
dig @127.0.0.1 -p "$PORT" example.com A +time=10 +tries=1 | grep -q 'status: REFUSED'
dig @127.0.0.1 -p "$PORT" example.com A +tcp +time=10 +tries=1 | grep -q 'status: REFUSED'
stop
if MOSDNS_ENTRY_TIMEOUT=invalid "$BIN" start -c "$D/config.yaml" > "$D/invalid.log" 2>&1; then echo 'invalid timeout accepted' >&2; exit 1; fi
grep -q 'invalid MOSDNS_ENTRY_TIMEOUT' "$D/invalid.log"
echo 'PASS default 5s, configurable 8s UDP/TCP, invalid timeout rejected'

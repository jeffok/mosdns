#!/bin/sh
# Requires a test image containing a long-running stub /usr/bin/bgp-agent.
# Does not use or touch any real RouterOS resources.
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
IMAGE=${MOSDNS_IMAGE:-mosdns:combined-supervisor-test}
P=mosdns-combined-test-$$
W=$(mktemp -d)
cleanup() { docker rm -f "$P" >/dev/null 2>&1 || :; rm -rf "$W"; }
trap cleanup EXIT INT TERM
mkdir -p "$W/rules" "$W/state" "$W/conf"
cp "$ROOT"/tests/fixtures/* "$W/rules/"
printf '%s\n' '{"confirm_client_ip":"127.0.0.1","local_ip":"192.0.2.44","next_hop":"192.0.2.44","dns_listen":"127.0.0.1:1053","http_listen":"0.0.0.0:9080","warm_domains":[]}' > "$W/conf/agent.json"
start() {
 docker run -d --name "$P" -e BGP_EMBEDDED=1 -e ENABLE_ROS_BGP=1 -e BGP_LOCAL_IP=192.0.2.44 -e BGP_AGENT_CONFIG=/test/agent.json -e RULES_SOURCES= -e CUSTOM_HOSTS_SOURCES= -v "$ROOT/scripts:/opt/mosdns:ro" -v "$W/rules:/etc/mosdns/rules" -v "$W/state:/data/state" -v "$W/conf:/test:ro" --entrypoint /opt/mosdns/entrypoint.sh "$IMAGE" >/dev/null
 for i in $(seq 1 30); do
   if docker exec "$P" sh -c '[ -s /data/state/bgp-agent.pid ] && [ -s /tmp/mosdns.pid ] && kill -0 "$(cat /data/state/bgp-agent.pid)" && kill -0 "$(cat /tmp/mosdns.pid)"' 2>/dev/null; then return 0; fi
   sleep 1
 done
 docker logs --tail 20 "$P" >&2; return 1
}
for process in bgp dns; do
 rm -f "$W/state/bgp-agent.pid"
 start
 if [ "$process" = bgp ]; then pidfile=/data/state/bgp-agent.pid; else pidfile=/tmp/mosdns.pid; fi
 docker exec "$P" sh -c 'kill -9 "$(cat "$1")"' _ "$pidfile"
 code=$(docker wait "$P")
 [ "$code" = 1 ] || { echo "$process death returned $code, expected 1" >&2; exit 1; }
 docker rm "$P" >/dev/null
done
start
docker stop -t 10 "$P" >/dev/null
code=$(docker inspect -f '{{.State.ExitCode}}' "$P")
[ "$code" = 0 ] || { echo "SIGTERM returned $code, expected 0" >&2; exit 1; }
docker rm "$P" >/dev/null
# A non-loopback source or direct agent prewarming must prevent startup.
for invalid in remote-client prewarm; do
 if [ "$invalid" = remote-client ]; then
  printf '%s\n' '{"confirm_client_ip":"192.0.2.22","local_ip":"192.0.2.44","next_hop":"192.0.2.44","dns_listen":"127.0.0.1:1053","http_listen":"0.0.0.0:9080","warm_domains":[]}' > "$W/conf/agent.json"
 else
  printf '%s\n' '{"confirm_client_ip":"127.0.0.1","local_ip":"192.0.2.44","next_hop":"192.0.2.44","dns_listen":"127.0.0.1:1053","http_listen":"0.0.0.0:9080","warm_domains":["example.com"]}' > "$W/conf/agent.json"
 fi
 start_invalid=$(docker run --rm -e BGP_EMBEDDED=1 -e ENABLE_ROS_BGP=1 -e BGP_LOCAL_IP=192.0.2.44 -e BGP_AGENT_CONFIG=/test/agent.json -e RULES_SOURCES= -e CUSTOM_HOSTS_SOURCES= -v "$ROOT/scripts:/opt/mosdns:ro" -v "$W/rules:/etc/mosdns/rules" -v "$W/state:/data/state" -v "$W/conf:/test:ro" --entrypoint /opt/mosdns/entrypoint.sh "$IMAGE" 2>&1 || :)
 printf '%s' "$start_invalid" | grep -q 'invalid combined BGP config'
done
echo 'PASS combined supervisor child failures, SIGTERM, and config checks'

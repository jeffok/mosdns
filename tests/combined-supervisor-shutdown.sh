#!/bin/sh
# Requires an image with a long-running stub bgp-agent. Exercises a stubborn
# DNS child without changing any production processes or network equipment.
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
IMAGE=${MOSDNS_IMAGE:-mosdns:combined-guard-isolated-test}
P=mosdns-shutdown-test-$$
W=$(mktemp -d)
cleanup() { docker rm -f "$P" >/dev/null 2>&1 || :; rm -rf "$W"; }
trap cleanup EXIT INT TERM
mkdir -p "$W/rules" "$W/state" "$W/conf"
cp "$ROOT"/tests/fixtures/* "$W/rules/"
printf '%s\n' '{"confirm_client_ip":"127.0.0.1","local_ip":"192.0.2.44","next_hop":"192.0.2.44","dns_listen":"127.0.0.1:1053","http_listen":"0.0.0.0:9080","warm_domains":[]}' > "$W/conf/agent.json"
cat > "$W/mosdns" <<'STUB'
#!/bin/sh
trap '' TERM
while :; do sleep 1; done
STUB
chmod +x "$W/mosdns"
docker run -d --name "$P" -e BGP_EMBEDDED=1 -e ENABLE_ROS_BGP=1 -e BGP_LOCAL_IP=192.0.2.44 -e BGP_AGENT_CONFIG=/test/agent.json -e RULES_SOURCES= -e CUSTOM_HOSTS_SOURCES= -v "$W/mosdns:/usr/bin/mosdns:ro" -v "$ROOT/scripts:/opt/mosdns:ro" -v "$W/rules:/etc/mosdns/rules" -v "$W/state:/data/state" -v "$W/conf:/test:ro" --entrypoint /opt/mosdns/entrypoint.sh "$IMAGE" >/dev/null
ready=0
for i in $(seq 1 30); do
 if docker exec "$P" sh -c '[ -s /data/state/bgp-agent.pid ] && [ -s /tmp/mosdns.pid ] && kill -0 "$(cat /data/state/bgp-agent.pid)" && kill -0 "$(cat /tmp/mosdns.pid)"' 2>/dev/null; then ready=1; break; fi
 sleep 1
done
[ "$ready" = 1 ] || { docker logs --tail 20 "$P" >&2; exit 1; }
docker exec "$P" sh -c 'kill -9 "$(cat /data/state/bgp-agent.pid)"'
# Give the supervisor enough time to detect the agent and signal the DNS child.
sleep 3
docker exec "$P" sh -c 'kill -0 "$(cat /tmp/mosdns.pid)" && kill -0 1' || { echo 'supervisor exited before stubborn DNS child' >&2; exit 1; }
docker exec "$P" sh -c 'kill -9 "$(cat /tmp/mosdns.pid)"'
code=$(docker wait "$P")
[ "$code" = 1 ] || { echo "supervisor returned $code, expected 1" >&2; exit 1; }
echo 'PASS supervisor waits for a DNS writer that ignores TERM'

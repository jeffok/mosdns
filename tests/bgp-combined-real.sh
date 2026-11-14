#!/bin/sh
set -eu
R=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
IMAGE=${MOSDNS_IMAGE:-mosdns:bgp-unified-final-test}
W=$(mktemp -d)
P=bgp-real-$$
cleanup(){ docker rm -f "$P-sut" "$P-cn" "$P-gl" >/dev/null 2>&1 || :; docker network rm "$P-net" >/dev/null 2>&1 || :; rm -rf "$W"; }
trap cleanup EXIT INT TERM
mkdir -p "$W/rules" "$W/state" "$W/conf"
cp "$R"/tests/fixtures/* "$W/rules/"
cat > "$W/conf/agent.json" <<'JSON'
{"local_ip":"172.31.245.22","peer_ip":"172.31.245.1","asn":64513,"peer_asn":64512,"next_hop":"172.31.245.22","dns_listen":"127.0.0.1:1053","http_listen":"0.0.0.0:9080","confirm_client_ip":"127.0.0.1","upstreams":["8.8.8.8:53"],"warm_domains":[],"cn_file":"/etc/mosdns/rules/IPv4.China.list","state_file":"/data/state/bgp-routes.json","ttl_max":300,"retain_seconds":86400,"confirm_ms":4500,"ack_max_age_ms":6000,"max_routes":512,"gc_dry_run":true}
JSON
docker network create --subnet 172.31.245.0/24 "$P-net" >/dev/null
docker run -d --name "$P-cn" --network "$P-net" --ip 172.31.245.2 --entrypoint /opt/tests/mock_upstream.sh -v "$R/tests:/opt/tests:ro" "$IMAGE" 192.168.1.10 53 >/dev/null
docker run -d --name "$P-gl" --network "$P-net" --ip 172.31.245.3 --entrypoint /opt/tests/mock_upstream.sh -v "$R/tests:/opt/tests:ro" "$IMAGE" 8.8.4.4 53 >/dev/null
docker run -d --name "$P-sut" --network "$P-net" --ip 172.31.245.22 -e BGP_EMBEDDED=1 -e ENABLE_ROS_BGP=1 -e BGP_LOCAL_IP=172.31.245.22 -e BGP_AGENT_CONFIG=/test/agent.json -e DNS_CN=172.31.245.2 -e DNS_GLOBAL=172.31.245.3 -e RULES_SOURCES= -e CUSTOM_HOSTS_SOURCES= -v "$W/rules:/etc/mosdns/rules" -v "$W/state:/data/state" -v "$W/conf:/test:ro" "$IMAGE" >/dev/null
ready=0
for i in $(seq 1 40); do
 if docker exec "$P-sut" dig @127.0.0.1 same.test A +short +time=1 2>/dev/null | grep -Fxq 192.168.1.10; then ready=1; break; fi
 sleep 1
done
if [ "$ready" != 1 ]; then docker logs --tail 30 "$P-sut"; exit 1; fi
printf 'domestic DNS and both processes started\n'
docker exec "$P-sut" dig @127.0.0.1 google.test A +time=6 | grep 'status: SERVFAIL'
printf 'global DNS failed closed without BGP peer\n'

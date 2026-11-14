#!/bin/sh
# End-to-end selected-response provenance and fail-closed validation.
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
IMAGE=${MOSDNS_IMAGE:-mosdns:bgp-tools-local}
P=mosdns-bgp-it-$$
W=$(mktemp -d)
cleanup() { docker rm -f "$P-sut" "$P-cn" "$P-global" "$P-ack" >/dev/null 2>&1 || :; docker network rm "$P-net" >/dev/null 2>&1 || :; rm -rf "$W"; }
trap cleanup EXIT INT TERM
mkdir -p "$W/rules" "$W/state" "$W/data"
cp "$ROOT"/tests/fixtures/* "$W/rules/"
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o "$W/stub" "$ROOT/tests/bgp_confirm_mock.go"
docker network create --subnet 172.31.244.0/24 "$P-net" >/dev/null
docker run -d --name "$P-cn" --network "$P-net" --ip 172.31.244.2 --entrypoint /opt/tests/mock_upstream.sh -v "$ROOT/tests:/opt/tests:ro" "$IMAGE" 192.0.2.11 53 >/dev/null
docker run -d --name "$P-global" --network "$P-net" --ip 172.31.244.3 --entrypoint /opt/tests/mock_upstream.sh -v "$ROOT/tests:/opt/tests:ro" "$IMAGE" 8.8.8.8 53 >/dev/null
docker run -d --name "$P-ack" --network "$P-net" --ip 172.31.244.10 --entrypoint /stub -v "$W/stub:/stub:ro" -v "$W/data:/data" "$IMAGE" >/dev/null
docker run -d --name "$P-sut" --network "$P-net" --ip 172.31.244.20 -e DNS_CN=172.31.244.2 -e DNS_GLOBAL=172.31.244.3 -e FALLBACK_STANDBY=true -e ENABLE_ROS_BGP=1 -e ROS_BGP_CONFIRM_URL=http://172.31.244.10:8080/confirm -e RULES_SOURCES= -e CUSTOM_HOSTS_SOURCES= -v "$ROOT/scripts:/opt/mosdns:ro" -v "$W/rules:/etc/mosdns/rules" -v "$W/state:/data/state" --entrypoint /opt/mosdns/entrypoint.sh "$IMAGE" >/dev/null
ready=0
for i in $(seq 1 30); do if docker exec "$P-sut" dig @127.0.0.1 same.test A +short +time=1 2>/dev/null | grep -Fxq 192.0.2.11; then ready=1; break; fi; sleep 1; done
[ "$ready" = 1 ] || { echo 'failed to start mosdns' >&2; exit 1; }
[ ! -e "$W/data/requests" ] || { echo 'losing global response incorrectly confirmed' >&2; exit 1; }
docker exec "$P-sut" dig @127.0.0.1 pin.test A +short +time=3 | grep -Fxq 192.0.2.10
[ ! -e "$W/data/requests" ] || { echo 'static hosts incorrectly confirmed' >&2; exit 1; }
docker exec "$P-sut" dig @127.0.0.1 google.test A +short +time=3 | grep -Fxq 8.8.8.8
[ "$(wc -l < "$W/data/requests" | tr -d ' ')" = 1 ]
grep -Fq '"domain":"google.test.","ips":["8.8.8.8"],"ttl":10' "$W/data/requests"
touch "$W/data/fail"
docker exec "$P-sut" dig @127.0.0.1 hard.test A +time=3 | grep -q 'status: SERVFAIL'
rm "$W/data/fail"
docker stop "$P-cn" >/dev/null
docker exec "$P-sut" dig @127.0.0.1 soft.test A +short +time=5 | grep -Fxq 8.8.8.8
grep -Fq '"domain":"soft.test.","ips":["8.8.8.8"],"ttl":10' "$W/data/requests"
# Each existing truthy IPv6 setting must fail before the combined policy starts.
for ipv6 in 1 true yes; do
  if docker run --rm -e RENDER_ONLY=1 -e ENABLE_ROS_BGP=1 -e ENABLE_IPV6="$ipv6" \
    -e ROS_BGP_CONFIRM_URL=http://127.0.0.1:9080/confirm \
    -v "$ROOT/scripts:/opt/mosdns:ro" -v "$W/rules:/etc/mosdns/rules" \
    -v "$W/state:/data/state" --entrypoint /opt/mosdns/entrypoint.sh "$IMAGE" \
    > "$W/ipv6-$ipv6.log" 2>&1; then
    echo "ENABLE_IPV6=$ipv6 unexpectedly accepted in BGP mode" >&2; exit 1
  fi
  grep -q 'ENABLE_ROS_BGP does not support ENABLE_IPV6' "$W/ipv6-$ipv6.log"
done
echo 'PASS selected source, DNS fallback, confirmation failure, and IPv6 rejection'

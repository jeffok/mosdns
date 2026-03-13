#!/bin/bash
# hkcloud OpenResty 验证脚本
# 在 hkcloud 同网段执行（如 222 @ 10.100.50.222），验证 DoH、订阅、443
# 用法：./verify-hkcloud.sh [OPENRESTY_IP]
# 默认 OPENRESTY_IP=10.100.50.253

set -euo pipefail
OR_IP="${1:-10.100.50.253}"
HOST_DOH="doh.jeffok.com"
HOST_OVPN="ovpn.jeffok.com"
DNS_QUERY_HEX="0000010000010000000000000377777706676f6f676c6503636f6d0000010001"

run_test() {
    local name="$1"
    local cmd="$2"
    echo "=== $name ==="
    if eval "$cmd"; then
        echo "OK"
    else
        echo "FAIL"
        return 1
    fi
    echo ""
}

echo "OpenResty IP: $OR_IP"
echo ""

# DoH 993
run_test "DoH 993" "curl -sk --connect-timeout 5 -H 'Host: $HOST_DOH' -H 'Content-Type: application/dns-message' --data-binary \"\$(echo $DNS_QUERY_HEX | xxd -r -p)\" \"https://$OR_IP:993/dns-query\" | xxd | head -5"

# 订阅 4443
run_test "订阅 4443 /sub/a4050c65.txt" "curl -sk --connect-timeout 5 -H 'Host: $HOST_OVPN' \"https://$OR_IP:4443/sub/a4050c65.txt\" | head -c 200"

# 443 DoH
run_test "443 DoH" "curl -sk --connect-timeout 5 -H 'Host: $HOST_DOH' -H 'Content-Type: application/dns-message' --data-binary \"\$(echo $DNS_QUERY_HEX | xxd -r -p)\" \"https://$OR_IP:443/dns-query\" | xxd | head -5"

echo "验证完成"

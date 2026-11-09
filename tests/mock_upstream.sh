#!/bin/sh
# mock_upstream.sh — 起一个「固定应答」的 DNS 服务，充当 mock 上游（P2）
#
# 为什么不自己写 DNS 协议：mosdns 本身就是现成的 DNS 服务端，
# 用 hosts 插件把整段测试域名固定到一个 IP，比手写 UDP 报文更可靠、更好维护。
#
# 用法：mock_upstream.sh <应答IP> <监听端口>
#   应答 IP 可以是 IPv4 或 IPv6；应答 TTL 固定 10s（hosts 插件行为）。
set -eu
IP=${1:?应答 IP}
PORT=${2:?监听端口}

workdir=$(mktemp -d)
cat > "$workdir/config.yaml" <<'CFGEOF'
log:
  level: info
  file: ""
plugins:
- tag: fixed
  type: hosts
  args:
    entries:
    # 用域名后缀 test，覆盖夹具里所有 *.test 域名：mock 只负责「固定应答」
    - "domain:test IP_PLACEHOLDER"
- tag: main
  type: sequence
  args:
  - exec: query_summary mock_query
  - exec: $fixed
  - matches: has_resp
    exec: accept
  - exec: reject 3
- tag: udp
  type: udp_server
  args:
    entry: main
    listen: 0.0.0.0:PORT_PLACEHOLDER
CFGEOF

sed -i -e "s#PORT_PLACEHOLDER#$PORT#" -e "s#IP_PLACEHOLDER#$IP#" "$workdir/config.yaml"
exec mosdns start -c "$workdir/config.yaml"
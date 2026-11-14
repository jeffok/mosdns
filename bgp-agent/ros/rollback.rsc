# Remove only isolated BGP validation objects. Does not touch household DNS,
# old mosdns, old gfw-proxy, OVPN, PPPoE, or imported VPN certificates.
/system scheduler remove [find where name="bgp-test-ack"]
/system script remove [find where name="bgp-test-ack"]
/ip firewall mangle remove [find where comment~"^BGP-TEST "]
/ip firewall connection remove [find where connection-mark="cm-bgp-test"]
/routing bgp connection remove [find where name="bgp-test"]
/routing bgp instance remove [find where name="bgp-test"]
/ip route remove [find where comment~"^BGP-TEST "]
/routing filter rule remove [find where chain="bgp-test-in"]
/routing filter rule remove [find where chain="bgp-test-out"]
/routing table remove [find where name="rt-bgp-test"]
:put "BGP-TEST routing and ACK objects removed"

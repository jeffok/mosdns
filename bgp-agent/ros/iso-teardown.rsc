# Disable first and refuse cleanup while the isolated writer still runs.
/system scheduler disable [find where name="szhome-mosdns-bgp-iso-writer"]
/routing bgp connection disable [find where name="szhome-mosdns-bgp-iso"]
:if ([:len [/system script job find where script="szhome-mosdns-bgp-iso-writer"]] != 0) do={ :error "isolated writer still running; retry teardown after it exits" }
/system scheduler remove [find where name="szhome-mosdns-bgp-iso-writer"]
/system script remove [find where name="szhome-mosdns-bgp-iso-writer"]
/ip firewall mangle remove [find where comment~"^BGP-ISO "]
# Only marks assigned by the .222-only test mangle are eligible for removal.
/ip firewall connection remove [find where connection-mark="cm-bgp-iso"]
/ip firewall filter remove [find where comment~"^BGP-ISO-GC:"]
/ip firewall filter remove [find where comment~"^BGP-ISO-GC-ANCHOR:"]
/routing bgp connection remove [find where name="szhome-mosdns-bgp-iso"]
/routing bgp instance remove [find where name="szhome-mosdns-bgp-iso"]
/ip route remove [find where routing-table="rt-bgp-test" and comment~"^BGP-ISO"]
/routing filter rule remove [find where chain="szhome-bgp-iso-in"]
/routing filter rule remove [find where chain="szhome-bgp-iso-out"]
/routing table remove [find where name="rt-bgp-test"]
:put "BGP-ISO isolated objects removed"

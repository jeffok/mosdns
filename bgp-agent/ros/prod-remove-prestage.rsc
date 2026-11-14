# Remove *unused pre-stage objects only*. Never use this to retire a live
# production system: guard/barrier cleanup requires the not-yet-wired lifecycle.
# Run prod-quiesce first; stop container-side BGP; inspect routes and DNS.
:if ([:len [/system script job find where script="mosdns-writer"]] != 0) do={ :error "writer still running" }
:if ([:len [/ip route find where comment~"^BGP-PROD-BH:"]] != 0) do={ :error "owned guards exist; do not remove without safe lifecycle" }
:if ([:len [/ip firewall filter find where comment~"^BGP-PROD-GC:"]] != 0) do={ :error "owned barriers exist; do not remove without safe lifecycle" }
:if ([:len [/routing bgp session find where name~"mosdns"]] != 0) do={ :error "BGP session still exists; wait for withdrawal" }
:if ([:len [/ip route find where routing-table="main" and active and bgp]] != 0) do={ :error "active main-table BGP routes exist; inspect before cleanup" }
:local peer [/routing bgp connection find where name="mosdns"]
:if ([:len $peer] > 1) do={ :error "ambiguous BGP connection" }
:if ([:len $peer] = 1) do={
    :if (([/routing bgp connection get $peer disabled] != true) || ([:tostr [/routing bgp connection get $peer comment]] != "mosdns: disabled until writer validated")) do={ :error "peer enabled or unexpected owner" }
}
:local ins [/routing filter rule find where chain="mosdns-in"]
:local outs [/routing filter rule find where chain="mosdns-out"]
:if (([:len $ins] != [:len [/routing filter rule find where chain="mosdns-in" and comment="mosdns: reject special"]] + [:len [/routing filter rule find where chain="mosdns-in" and comment="mosdns: hosts via ovpn"]]) || ([:len $outs] != [:len [/routing filter rule find where chain="mosdns-out" and comment="mosdns: no exports"]])) do={ :error "foreign filter rule in owned namespace" }
/system scheduler remove [find where name="mosdns-writer" and comment="mosdns: guarded writer"]
/system script remove [find where name="mosdns-writer" and comment="mosdns: guarded writer"]
:if ([:len $peer] = 1) do={ /routing bgp connection remove $peer }
/routing bgp instance remove [find where name="mosdns" and comment="mosdns: local instance"]
/routing filter rule remove [find where chain="mosdns-in" and comment="mosdns: reject special"]
/routing filter rule remove [find where chain="mosdns-in" and comment="mosdns: hosts via ovpn"]
/routing filter rule remove [find where chain="mosdns-out" and comment="mosdns: no exports"]
:put "mosdns unused pre-stage removed; DNS and routes unchanged"

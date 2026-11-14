# Remove *unused pre-stage objects only*. Never use this to retire a live
# production system: guard/barrier cleanup requires the not-yet-wired lifecycle.
# Run prod-quiesce first; stop container-side BGP; inspect routes and DNS.
:if ([:len [/system script job find where script="szhome-mosdns-bgp-writer"]] != 0) do={ :error "writer still running" }
:if ([:len [/ip route find where comment~"^BGP-PROD-BH:"]] != 0) do={ :error "owned guards exist; do not remove without safe lifecycle" }
:if ([:len [/ip firewall filter find where comment~"^BGP-PROD-GC:"]] != 0) do={ :error "owned barriers exist; do not remove without safe lifecycle" }
:if ([:len [/routing bgp session find where name~"szhome-mosdns-bgp"]] != 0) do={ :error "BGP session still exists; wait for withdrawal" }
:if ([:len [/ip route find where routing-table="main" and active and bgp]] != 0) do={ :error "active main-table BGP routes exist; inspect before cleanup" }
:local peer [/routing bgp connection find where name="szhome-mosdns-bgp"]
:if ([:len $peer] > 1) do={ :error "ambiguous BGP connection" }
:if ([:len $peer] = 1) do={
    :if (([/routing bgp connection get $peer disabled] != true) || ([:tostr [/routing bgp connection get $peer comment]] != "szhome-mosdns-bgp: disabled until writer validated")) do={ :error "peer enabled or unexpected owner" }
}
:local ins [/routing filter rule find where chain="szhome-mosdns-bgp-in"]
:local outs [/routing filter rule find where chain="szhome-mosdns-bgp-out"]
:if (([:len $ins] != [:len [/routing filter rule find where chain="szhome-mosdns-bgp-in" and comment="szhome-mosdns-bgp: reject special"]] + [:len [/routing filter rule find where chain="szhome-mosdns-bgp-in" and comment="szhome-mosdns-bgp: hosts via ovpn"]]) || ([:len $outs] != [:len [/routing filter rule find where chain="szhome-mosdns-bgp-out" and comment="szhome-mosdns-bgp: no exports"]])) do={ :error "foreign filter rule in owned namespace" }
/system scheduler remove [find where name="szhome-mosdns-bgp-writer" and comment="szhome-mosdns-bgp: guarded writer"]
/system script remove [find where name="szhome-mosdns-bgp-writer" and comment="szhome-mosdns-bgp: guarded writer"]
:if ([:len $peer] = 1) do={ /routing bgp connection remove $peer }
/routing bgp instance remove [find where name="szhome-mosdns-bgp" and comment="szhome-mosdns-bgp: local instance"]
/routing filter rule remove [find where chain="szhome-mosdns-bgp-in" and comment="szhome-mosdns-bgp: reject special"]
/routing filter rule remove [find where chain="szhome-mosdns-bgp-in" and comment="szhome-mosdns-bgp: hosts via ovpn"]
/routing filter rule remove [find where chain="szhome-mosdns-bgp-out" and comment="szhome-mosdns-bgp: no exports"]
:put "szhome-mosdns-bgp unused pre-stage removed; DNS and routes unchanged"

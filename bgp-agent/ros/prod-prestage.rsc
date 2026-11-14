# SZHOME MOSDNS BGP -- disabled pre-stage ONLY (RouterOS 7.24.6).
# Installing this file does not enable a BGP peer, scheduler or household DNS.
# Do not enable the peer until the shared guard writer and crash recovery have
# been implemented and validated. No REST service or credential is required.
# Names are reserved for this service; abort on pre-existing objects.
:if ([:len [/container find where name="mosdns"]] != 1) do={ :error "expected exactly one mosdns container" }
:if ([:len [/interface veth find where name="veth-mosdns"]] != 1) do={ :error "mosdns veth not found" }
:if ([:tostr [/interface veth get [find where name="veth-mosdns"] address]] != "192.168.88.222/24") do={ :error "mosdns is not at expected .222/24" }
:if ([:len [/interface find where name="ovpn-szof"]] != 1) do={ :error "OVPN interface missing" }
:if ([:len [/interface find where name="pppoe-cn"]] != 1) do={ :error "telecom interface missing" }
:if ([:len [/routing table find where name="main"]] != 1) do={ :error "main routing table missing" }
:if ([:len [/routing bgp connection find where name="mosdns"]] != 0) do={ :error "BGP connection name already in use" }
:if ([:len [/routing bgp instance find where name="mosdns"]] != 0) do={ :error "BGP instance name already in use" }
:if (([:len [/routing filter rule find where chain="mosdns-in"]] != 0) || ([:len [/routing filter rule find where chain="mosdns-out"]] != 0)) do={ :error "BGP filter chain name already in use" }
:if (([:len [/system script find where name="mosdns-writer"]] != 0) || ([:len [/system scheduler find where name="mosdns-writer"]] != 0)) do={ :error "BGP writer name already in use" }
:if ([:len [/ip route find where comment~"^BGP-PROD-BH:"]] != 0) do={ :error "existing production guard namespace; reconcile first" }
:if ([:len [/ip firewall filter find where comment~"^BGP-PROD-GC:"]] != 0) do={ :error "existing production barrier namespace; reconcile first" }

# Reject special-use prefixes, bootstrap resolver hosts and default/aggregates.
# The agent must separately reject all non-eligible/CN responses. The routing
# filter alone is not a complete CN database or an authentication mechanism.
/routing filter rule add chain=mosdns-in comment="mosdns: reject special" rule="if (dst in 0.0.0.0/8 || dst in 10.0.0.0/8 || dst in 100.64.0.0/10 || dst in 127.0.0.0/8 || dst in 169.254.0.0/16 || dst in 172.16.0.0/12 || dst in 192.168.0.0/16 || dst in 192.0.0.0/24 || dst in 192.0.2.0/24 || dst in 198.18.0.0/15 || dst in 198.51.100.0/24 || dst in 203.0.113.0/24 || dst in 224.0.0.0/3 || dst in 8.8.8.8/32 || dst in 1.1.1.1/32 || dst in 183.56.180.152/32) {reject}"
/routing filter rule add chain=mosdns-in comment="mosdns: hosts via ovpn" rule="if (afi ipv4 && dst-len == 32) {set gw ovpn-szof; set distance 20; accept} else {reject}"
/routing filter rule add chain=mosdns-out comment="mosdns: no exports" rule="reject"
/routing bgp instance add name=mosdns as=64512 router-id=192.168.88.254 routing-table=main comment="mosdns: local instance"
/routing bgp connection add name=mosdns instance=mosdns local.role=ebgp local.address=192.168.88.254 remote.address=192.168.88.222/32 remote.as=64513 routing-table=main afi=ip input.filter=mosdns-in input.limit-process-routes-ipv4=512 output.filter-chain=mosdns-out hold-time=30s keepalive-time=10s disabled=yes comment="mosdns: disabled until writer validated"
:put "mosdns pre-staged; peer DISABLED, no DNS or routing cutover performed"

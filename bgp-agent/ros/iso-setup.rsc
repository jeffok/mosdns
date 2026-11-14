# Isolated .222-only RouterOS BGP test. Does not alter main routes or DNS.
# Stops if names collide. The peer and writer scheduler are created disabled.
:if ([:len [/routing table find where name="rt-bgp-test"]] != 0) do={ :error "test table exists" }
:if ([:len [/routing bgp connection find where name="szhome-mosdns-bgp-iso"]] != 0) do={ :error "test peer exists" }
:if ([:len [/routing bgp instance find where name="szhome-mosdns-bgp-iso"]] != 0) do={ :error "test instance exists" }
:if ([:len [/routing filter rule find where chain="szhome-bgp-iso-in"]] != 0) do={ :error "test input filter exists" }
:if ([:len [/routing filter rule find where chain="szhome-bgp-iso-out"]] != 0) do={ :error "test output filter exists" }
:if ([:len [/system scheduler find where name="szhome-mosdns-bgp-iso-writer"]] != 0) do={ :error "test scheduler exists" }
:if ([:len [/ip firewall filter find where comment~"^BGP-ISO-"]] != 0) do={ :error "test firewall namespace exists" }
:if ([:len [/ip route find where comment~"^BGP-ISO-"]] != 0) do={ :error "test route namespace exists" }
:if ([:len [/ip firewall mangle find where comment~"^BGP-ISO "]] != 0) do={ :error "test mangle namespace exists" }
:if ([:len [/ip firewall connection find where connection-mark="cm-bgp-iso"]] != 0) do={ :error "test connection mark already in use" }
:if ([:len [/ip firewall mangle find where comment="OVPN-proxy mark-conn"]] != 1) do={ :error "OVPN mangle anchor missing" }
:if ([:len [/interface find where name="ovpn-szof"]] != 1) do={ :error "OVPN interface missing" }
:if ([:len [/interface find where name="pppoe-cn"]] != 1) do={ :error "telecom interface missing" }
:if ([:len [/container find where name="mosdns"]] != 1) do={ :error "expected one container" }
:if ([:len [/interface veth find where name="veth-mosdns"]] != 1) do={ :error "missing container veth" }
:if ([:tostr [/interface veth get [find where name="veth-mosdns"] address]] != "192.168.88.222/24") do={ :error "container is not .222" }
:if ([:len [/interface bridge port find where interface="veth-mosdns" and bridge="br-lan"]] != 1) do={ :error "container veth not on br-lan" }
:if ([:typeof [:find [:tostr [/ip dns get servers]] "192.168.88.222"]] != "nil") do={ :error "household DNS points at test container" }
/routing table add name=rt-bgp-test fib comment="BGP-ISO: .222 test only"
/routing filter rule add chain=szhome-bgp-iso-in comment="BGP-ISO reject special" rule="if (dst in 0.0.0.0/8 || dst in 10.0.0.0/8 || dst in 100.64.0.0/10 || dst in 127.0.0.0/8 || dst in 169.254.0.0/16 || dst in 172.16.0.0/12 || dst in 192.168.0.0/16 || dst in 192.0.0.0/24 || dst in 192.0.2.0/24 || dst in 198.18.0.0/15 || dst in 198.51.100.0/24 || dst in 203.0.113.0/24 || dst in 224.0.0.0/3 || dst in 8.8.8.8/32 || dst in 1.1.1.1/32 || dst in 183.56.180.152/32) {reject}"
/routing filter rule add chain=szhome-bgp-iso-in comment="BGP-ISO hosts via OVPN" rule="if (afi ipv4 && dst-len == 32) {set gw ovpn-szof; set distance 20; accept} else {reject}"
/routing filter rule add chain=szhome-bgp-iso-out comment="BGP-ISO no exports" rule="reject"
/routing bgp instance add name=szhome-mosdns-bgp-iso as=64512 router-id=192.168.88.254 routing-table=rt-bgp-test comment="BGP-ISO instance"
/routing bgp connection add name=szhome-mosdns-bgp-iso instance=szhome-mosdns-bgp-iso local.role=ebgp local.address=192.168.88.254 remote.address=192.168.88.222/32 remote.as=64513 routing-table=rt-bgp-test afi=ip input.filter=szhome-bgp-iso-in input.limit-process-routes-ipv4=512 output.filter-chain=szhome-bgp-iso-out hold-time=30s keepalive-time=10s disabled=yes comment="BGP-ISO .222 peer"
# Explicit resolver bootstrap and fail-closed test default; all routes in test table.
/ip route add dst-address=0.0.0.0/0 blackhole routing-table=rt-bgp-test distance=250 comment="BGP-ISO default blackhole"
/ip route add dst-address=8.8.8.8/32 gateway=ovpn-szof routing-table=rt-bgp-test distance=1 comment="BGP-ISO bootstrap Google"
/ip route add dst-address=8.8.8.8/32 blackhole routing-table=rt-bgp-test distance=240 comment="BGP-ISO bootstrap Google guard"
/ip route add dst-address=1.1.1.1/32 gateway=ovpn-szof routing-table=rt-bgp-test distance=1 comment="BGP-ISO bootstrap Cloudflare"
/ip route add dst-address=1.1.1.1/32 blackhole routing-table=rt-bgp-test distance=240 comment="BGP-ISO bootstrap Cloudflare guard"
/ip route add dst-address=119.29.29.29/32 gateway=pppoe-cn routing-table=rt-bgp-test distance=1 comment="BGP-ISO bootstrap domestic"
/ip firewall mangle add chain=prerouting action=mark-connection new-connection-mark=cm-bgp-iso passthrough=yes src-address=192.168.88.222/32 in-interface=br-lan dst-address-type=!local connection-state=new connection-mark=no-mark place-before=[find where comment="OVPN-proxy mark-conn"] comment="BGP-ISO mark .222"
/ip firewall mangle add chain=prerouting action=mark-routing new-routing-mark=rt-bgp-test passthrough=no src-address=192.168.88.222/32 in-interface=br-lan dst-address-type=!local connection-mark=cm-bgp-iso place-before=[find where comment="OVPN-proxy mark-conn"] comment="BGP-ISO route .222"
# Passthrough anchors have no forwarding effect. Test drop rules installed by
# iso-writer always additionally match source 192.168.88.222/32.
:foreach chain in={"forward";"output"} do={
    :local first [/ip firewall filter find where chain=$chain]
    :if ([:len $first] > 0) do={
        /ip firewall filter add chain=$chain action=passthrough place-before=($first->0) comment=("BGP-ISO-GC-ANCHOR:" . $chain)
    } else={
        /ip firewall filter add chain=$chain action=passthrough comment=("BGP-ISO-GC-ANCHOR:" . $chain)
    }
}
/system scheduler add name=szhome-mosdns-bgp-iso-writer interval=1s on-event="szhome-mosdns-bgp-iso-writer" policy=read,write,test start-time=startup disabled=yes comment="BGP-ISO disabled writer"
:put "BGP-ISO staged; peer and scheduler disabled"

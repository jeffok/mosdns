# RouterOS 7.24.6: isolated IPv4 BGP validation, source 192.168.88.223 only.
# The parent operator is the only router writer. Review then import explicitly.
# Precondition: agent .223 is reachable on br-lan, OVPN and pppoe-cn are running.
# This file does not change household DNS, DHCP, existing OVPN-proxy rules,
# existing FastTrack, certificates, or the old mosdns container.
# Objects must not already exist: use rollback.rsc before re-creating this setup.

:if ([:len [/routing table find where name="rt-bgp-test"]] != 0) do={ :error "rt-bgp-test already exists; inspect it before setup" }
:if ([:len [/routing bgp connection find where name="bgp-test"]] != 0) do={ :error "bgp-test connection already exists" }
:if ([:len [/ip firewall mangle find where comment="OVPN-proxy mark-conn"]] != 1) do={ :error "expected exactly one existing OVPN-proxy mark-conn anchor" }
:if ([:len [/interface find where name="ovpn-szof"]] != 1) do={ :error "OVPN interface missing" }
:if ([:len [/interface find where name="pppoe-cn"]] != 1) do={ :error "pppoe-cn missing" }

/routing table add name=rt-bgp-test fib comment="BGP-TEST isolated agent source"
/routing filter rule add chain=bgp-test-in comment="BGP-TEST reject private/reserved" rule="if (dst in 0.0.0.0/8 || dst in 10.0.0.0/8 || dst in 100.64.0.0/10 || dst in 127.0.0.0/8 || dst in 169.254.0.0/16 || dst in 172.16.0.0/12 || dst in 192.168.0.0/16 || dst in 192.0.0.0/24 || dst in 192.0.2.0/24 || dst in 198.18.0.0/15 || dst in 198.51.100.0/24 || dst in 203.0.113.0/24 || dst in 224.0.0.0/3) {reject}"
/routing filter rule add chain=bgp-test-in comment="BGP-TEST accept host routes via OVPN" rule="if (afi ipv4 && dst-len == 32) {set gw ovpn-szof; set distance 20; accept} else {reject}"
/routing filter rule add chain=bgp-test-out comment="BGP-TEST no route export" rule="reject"
/routing bgp instance add name=bgp-test as=64512 router-id=192.168.88.254 routing-table=rt-bgp-test
/routing bgp connection add name=bgp-test instance=bgp-test local.role=ebgp local.address=192.168.88.254 remote.address=192.168.88.223/32 remote.as=64513 routing-table=rt-bgp-test afi=ip input.filter=bgp-test-in input.limit-process-routes-ipv4=512 output.filter-chain=bgp-test-out hold-time=30s keepalive-time=10s comment="BGP-TEST isolated fixed neighbor"

# Bootstrap DNS always goes through the same OVPN used by the current resolver.
# Do not let BGP advertisements replace these routes with an equal-distance path.
/ip route add dst-address=0.0.0.0/0 gateway=pppoe-cn routing-table=rt-bgp-test distance=250 comment="BGP-TEST fallback direct"
/ip route add dst-address=8.8.8.8/32 gateway=ovpn-szof routing-table=rt-bgp-test distance=1 comment="BGP-TEST bootstrap Google DNS"
/ip route add dst-address=1.1.1.1/32 gateway=ovpn-szof routing-table=rt-bgp-test distance=1 comment="BGP-TEST bootstrap Cloudflare DNS"

# A connection mark also keeps this source out of existing no-mark FastTrack.
# The routing rule precedes old address-list mangle, so old REST entries cannot
# make the BGP test appear successful while it is actually using rt-ovpn.
/ip firewall mangle add chain=prerouting action=mark-connection new-connection-mark=cm-bgp-test passthrough=yes src-address=192.168.88.223/32 in-interface=br-lan dst-address-type=!local connection-state=new connection-mark=no-mark place-before=[find where comment="OVPN-proxy mark-conn"] comment="BGP-TEST mark agent connections"
/ip firewall mangle add chain=prerouting action=mark-routing new-routing-mark=rt-bgp-test passthrough=no src-address=192.168.88.223/32 in-interface=br-lan dst-address-type=!local connection-mark=cm-bgp-test place-before=[find where comment="OVPN-proxy mark-conn"] comment="BGP-TEST lookup isolated table"

# Install ack-script.rsc separately. Enable only after the container and script
# have been inspected; this file intentionally leaves the scheduler disabled.
/system scheduler add name=bgp-test-ack interval=1s on-event="bgp-test-ack" policy=read,test start-time=startup disabled=yes comment="BGP-TEST active-route ACK"

:put "BGP-TEST setup complete; ACK scheduler remains disabled"

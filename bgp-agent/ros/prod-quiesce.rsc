# Quiesce only objects owned by szhome-mosdns-bgp. Never remove a guard here.
# This is not a DNS rollback: choose a verified resolver separately. A running
# writer is allowed to finish before disabling the peer; do not run unrelated jobs.
:local sched [/system scheduler find where name="szhome-mosdns-bgp-writer"]
:if ([:len $sched] > 1) do={ :error "ambiguous writer scheduler" }
:if ([:len $sched] = 1) do={
    :if ([:tostr [/system scheduler get $sched comment]] != "szhome-mosdns-bgp: guarded writer") do={ :error "unexpected scheduler owner" }
    /system scheduler disable $sched
}
:if ([:len [/system script job find where script="szhome-mosdns-bgp-writer"]] != 0) do={ :error "writer still running; wait and rerun" }
:local peer [/routing bgp connection find where name="szhome-mosdns-bgp"]
:if ([:len $peer] > 1) do={ :error "ambiguous BGP connection" }
:if ([:len $peer] = 1) do={
    :if ([:tostr [/routing bgp connection get $peer comment]] != "szhome-mosdns-bgp: disabled until writer validated") do={ :error "unexpected BGP connection owner" }
    /routing bgp connection disable $peer
}
:put "szhome-mosdns-bgp scheduler and peer disabled; guards retained, DNS unchanged"

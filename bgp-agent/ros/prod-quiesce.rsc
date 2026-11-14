# Quiesce only objects owned by mosdns. Never remove a guard here.
# This is not a DNS rollback: choose a verified resolver separately. A running
# writer is allowed to finish before disabling the peer; do not run unrelated jobs.
:local sched [/system scheduler find where name="mosdns-writer"]
:if ([:len $sched] > 1) do={ :error "ambiguous writer scheduler" }
:if ([:len $sched] = 1) do={
    :if ([:tostr [/system scheduler get $sched comment]] != "mosdns: guarded writer") do={ :error "unexpected scheduler owner" }
    /system scheduler disable $sched
}
:if ([:len [/system script job find where script="mosdns-writer"]] != 0) do={ :error "writer still running; wait and rerun" }
:local peer [/routing bgp connection find where name="mosdns"]
:if ([:len $peer] > 1) do={ :error "ambiguous BGP connection" }
:if ([:len $peer] = 1) do={
    :if ([:tostr [/routing bgp connection get $peer comment]] != "mosdns: disabled until writer validated") do={ :error "unexpected BGP connection owner" }
    /routing bgp connection disable $peer
}
:put "mosdns scheduler and peer disabled; guards retained, DNS unchanged"

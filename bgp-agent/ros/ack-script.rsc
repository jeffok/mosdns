# RouterOS 7.24.6. Install this named script before enabling its scheduler.
# No RouterOS credentials are sent to the BGP agent.
# The agent accepts ACK only from 192.168.88.254 with its current epoch
# and a fresh, single-use snapshot_id issued by /desired.
# /ip route active is a control-plane observation, not a packet-delivery proof.

/system script add name=bgp-test-ack policy=read,test comment="BGP-TEST active-route ACK" source={
    # Skip overlapping runs without an invalid top-level :return. Only the
    # running job may send a snapshot; old ACKs expire at the agent.
    :if ([/system script job print count-only where script=[:jobname]] <= 1) do={
        :do {
            :local started [:timestamp]
            :local fetched [/tool fetch url="http://192.168.88.223:9080/desired" src-address=192.168.88.254 http-max-redirect-count=0 duration=3s idle-timeout=2s output=user as-value]
            :if (($fetched->"status") != "finished") do={ :error "desired fetch incomplete" }
            :local desired [:deserialize from=json value=($fetched->"data")]
            :local epoch ($desired->"epoch")
            :local snapshotID ($desired->"snapshot_id")
            :local prefixes ($desired->"prefixes")
            :if (([:typeof $epoch] != "str") || ([:typeof $snapshotID] != "str") || ([:typeof $prefixes] != "array")) do={ :error "invalid desired snapshot types" }
            :if (([:len $epoch] != 24) || ([:len $snapshotID] != 24) || ([:len $prefixes] > 512)) do={ :error "invalid desired snapshot lengths" }
            :if ((($epoch ~ "^[0-9a-f]{24}") = false) || (($snapshotID ~ "^[0-9a-f]{24}") = false)) do={ :error "invalid desired snapshot identifiers" }

            # Read once, preserve the active/BGP/table and both gateway checks.
            # Key the in-memory set by a canonical IPv4 /32 prefix; a route
            # displayed as an unqualified host address is also a /32.
            :local snapshotStarted [:timestamp]
            :local snapshot [/ip route print detail as-value where routing-table="rt-bgp-test" and active and bgp]
            :local activeSet [:toarray ""]
            :foreach route in=$snapshot do={
                :if (([:tostr ($route->"gateway")] = "ovpn-szof") && ([:tostr ($route->"immediate-gw")] = "ovpn-szof")) do={
                    :local key [:tostr ($route->"dst-address")]
                    :if ([:find $key "/"] = nil) do={ :set key ($key . "/32") }
                    :set ($activeSet->$key) true
                }
            }
            :local accepted [:toarray ""]
            :foreach prefix in=$prefixes do={
                :local key [:tostr $prefix]
                :if ([:find $key "/"] = nil) do={ :set key ($key . "/32") }
                :if (($activeSet->$key) = true) do={ :set accepted ($accepted, $prefix) }
            }

            # A slow scan may describe a FIB that has already changed.
            :if (([:timestamp] - $snapshotStarted) > 1s) do={ :error "route snapshot too old" }
            :if (([:timestamp] - $started) > 2s) do={ :error "desired snapshot too old" }
            :local payload [:serialize to=json options=json.no-string-conversion value={"epoch"=$epoch;"snapshot_id"=$snapshotID;"prefixes"=$accepted}]
            :local posted [/tool fetch url="http://192.168.88.223:9080/ack" src-address=192.168.88.254 http-method=post http-header-field="Content-Type:application/json" http-data=$payload http-max-redirect-count=0 duration=3s idle-timeout=2s output=none as-value]
            :if (($posted->"status") != "finished") do={ :error "ack post incomplete" }
            :local ended [:timestamp]
            :local elapsed ($ended - $started)
            # Sample rather than maintaining a global that may not persist
            # across scheduler runs. Skipped overlaps can skip sample slots.
            :if ($elapsed > 500ms) do={
                :local sched [/system scheduler find where name="bgp-test-ack"]
                :if ([:len $sched] = 1) do={
                    :local runs [/system scheduler get $sched run-count]
                    :if (($runs % 60) = 0) do={
                        :log warning ("BGP-TEST ACK slow elapsed=" . $elapsed . " routes=" . [:len $snapshot] . " desired=" . [:len $prefixes] . " accepted=" . [:len $accepted])
                    }
                }
            }
        } on-error={
            # Do not retain fresh ACKs locally; agent ACK timestamps expire.
            # An outage should fail closed without flooding logs every second.
        }
    }
}

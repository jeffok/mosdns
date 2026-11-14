# Experimental RouterOS 7.24.6 script. DO NOT enable together with bgp-test-ack.
# Scope: rt-bgp-test and the existing 192.168.88.223 validation agent only.
# Named static guard routes survive an orderly RouterOS reboot. A configuration
# readback cannot prove that an abrupt power loss cannot lose the last write.
# No automatic guard deletion: stale guards deliberately consume the cap.
# This script is not an atomic forwarding or power-loss persistence barrier.

/system script add name=bgp-test-ack-blackhole policy=read,write,test comment="BGP-TEST guarded ACK experiment" source={
    :if ([/system script job print count-only where script=[:jobname]] <= 1) do={
        :do {
            :local started [:timestamp]
            :local fetched [/tool fetch url="http://192.168.88.223:9080/desired" src-address=192.168.88.254 http-max-redirect-count=0 duration=3s idle-timeout=2s output=user as-value]
            :if (($fetched->"status") != "finished") do={ :error "desired fetch incomplete" }
            :local desired [:deserialize from=json value=($fetched->"data")]
            :local epoch ($desired->"epoch")
            :local snapshotID ($desired->"snapshot_id")
            :local prefixes ($desired->"prefixes")
            :if (([:typeof $epoch] != "str") || ([:typeof $snapshotID] != "str") || ([:typeof $prefixes] != "array")) do={ :error "invalid desired types" }
            :if (([:len $epoch] != 24) || ([:len $snapshotID] != 24) || ([:len $prefixes] > 512)) do={ :error "invalid desired lengths" }
            :if ((($epoch ~ "^[0-9a-f]{24}") = false) || (($snapshotID ~ "^[0-9a-f]{24}") = false)) do={ :error "invalid desired identifiers" }

            # An active BGP /32 from the dedicated table is the sole source of
            # permissible destinations; its OVPN gateway must be exact.
            :local before [/ip route print detail as-value where routing-table="rt-bgp-test" and active and bgp]
            :local activeSet [:toarray ""]
            :foreach route in=$before do={
                :if (([:tostr ($route->"gateway")] = "ovpn-szof") && ([:tostr ($route->"immediate-gw")] = "ovpn-szof") && (($route->"distance") = 20)) do={
                    :local key [:tostr ($route->"dst-address")]
                    :if ([:find $key "/"] = nil) do={ :set key ($key . "/32") }
                    :set ($activeSet->$key) true
                }
            }

            # Count every object using this ownership namespace, regardless of
            # table or flags. A conflicting/duplicated owner name fails closed.
            :local owned [/ip route print detail as-value where comment~"^BGP-TEST-BH:"]
            :if ([:len $owned] > 512) do={ :error "guard route limit exceeded" }
            :local ownerCount [:toarray ""]
            :foreach route in=$owned do={
                :local comment [:tostr ($route->"comment")]
                :local count ($ownerCount->$comment)
                :if ([:typeof $count] = "nothing") do={ :set count 0 }
                :set ($ownerCount->$comment) ($count + 1)
            }
            :local guardCount [:len $owned]
            :foreach prefix in=$prefixes do={
                # RouterOS JSON can deserialize a CIDR string as ip-prefix.
                :local prefixType [:typeof $prefix]
                :if (($prefixType != "str") && ($prefixType != "ip-prefix")) do={ :error "invalid desired prefix type" }
                :local key [:tostr $prefix]
                :local slash [:find $key "/"]
                :if ($slash = nil) do={
                    :if ($prefixType != "ip-prefix") do={ :error "missing host mask" }
                    :set key ($key . "/32")
                    :set slash [:find $key "/"]
                }
                :if ([:pick $key $slash [:len $key]] != "/32") do={ :error "non-host desired prefix" }
                :local ipText [:pick $key 0 $slash]
                :local ip [:toip $ipText]
                :if (([:typeof $ip] != "ip") || ([:tostr $ip] != $ipText)) do={ :error "invalid or noncanonical IPv4 desired prefix" }
                :local comment ("BGP-TEST-BH:" . $key)
                :local count ($ownerCount->$comment)
                :if ([:typeof $count] = "nothing") do={ :set count 0 }
                :if ($count > 1) do={ :error "duplicate guard owner" }
                :if (($activeSet->$key = true) && ($count = 0) && ($guardCount < 512)) do={
                    /ip route add dst-address=$key routing-table="rt-bgp-test" blackhole distance=240 comment=$comment
                    :set ($ownerCount->$comment) 1
                    :set guardCount ($guardCount + 1)
                }
            }

            # Read back *all* namespace objects and only enabled, static,
            # blackhole, distance-240 routes in the test table. Matching .id,
            # exact comment and canonical destination prevents accepting a
            # foreign or malformed object. No route is modified or deleted.
            :local verifyStarted [:timestamp]
            :local allOwned [/ip route print detail as-value where comment~"^BGP-TEST-BH:"]
            :if ([:len $allOwned] > 512) do={ :error "guard route limit exceeded after writes" }
            :local good [/ip route print detail as-value where routing-table="rt-bgp-test" and static and blackhole and !disabled and distance=240 and comment~"^BGP-TEST-BH:"]
            :local goodIds [:toarray ""]
            :foreach route in=$good do={
                :local id [:tostr ($route->".id")]
                :set ($goodIds->$id) true
            }
            :local verified [:toarray ""]
            :local seen [:toarray ""]
            :foreach route in=$allOwned do={
                :local comment [:tostr ($route->"comment")]
                :if (($seen->$comment) = true) do={ :error "duplicate guard owner on readback" }
                :set ($seen->$comment) true
                :local key [:tostr ($route->"dst-address")]
                :if ([:find $key "/"] = nil) do={ :set key ($key . "/32") }
                :local id [:tostr ($route->".id")]
                :if (($comment = ("BGP-TEST-BH:" . $key)) && (($goodIds->$id) = true)) do={
                    :set ($verified->$key) true
                }
            }
            # Recheck active BGP after the static route writes; an inactive BGP
            # path or active blackhole is never acknowledged as a live route.
            :local after [/ip route print detail as-value where routing-table="rt-bgp-test" and active and bgp]
            :local activeAfter [:toarray ""]
            :local bgpIds [:toarray ""]
            :foreach route in=$after do={
                :if (([:tostr ($route->"gateway")] = "ovpn-szof") && ([:tostr ($route->"immediate-gw")] = "ovpn-szof") && (($route->"distance") = 20)) do={
                    :local key [:tostr ($route->"dst-address")]
                    :if ([:find $key "/"] = nil) do={ :set key ($key . "/32") }
                    :set ($activeAfter->$key) true
                    :local id [:tostr ($route->".id")]
                    :set ($bgpIds->$id) true
                }
            }
            # Reject same-prefix active competitors, including ECMP.
            :local allActive [/ip route print detail as-value where routing-table="rt-bgp-test" and active]
            :local conflict [:toarray ""]
            :foreach route in=$allActive do={
                :local id [:tostr ($route->".id")]
                :if (($bgpIds->$id) != true) do={
                    :local key [:tostr ($route->"dst-address")]
                    :if ([:find $key "/"] = nil) do={ :set key ($key . "/32") }
                    :set ($conflict->$key) true
                }
            }
            :local accepted [:toarray ""]
            :foreach prefix in=$prefixes do={
                :local key [:tostr $prefix]
                :if ([:find $key "/"] = nil) do={ :set key ($key . "/32") }
                :if ((($verified->$key) = true) && (($activeAfter->$key) = true) && (($conflict->$key) != true)) do={ :set accepted ($accepted, $key) }
            }
            :if (([:timestamp] - $verifyStarted) > 1s) do={ :error "route readback too old" }
            :if (([:timestamp] - $started) > 2s) do={ :error "desired snapshot too old" }
            :local payload [:serialize to=json options=json.no-string-conversion value={"epoch"=$epoch;"snapshot_id"=$snapshotID;"prefixes"=$accepted}]
            :local posted [/tool fetch url="http://192.168.88.223:9080/ack" src-address=192.168.88.254 http-method=post http-header-field="Content-Type:application/json" http-data=$payload http-max-redirect-count=0 duration=3s idle-timeout=2s output=none as-value]
            :if (($posted->"status") != "finished") do={ :error "ack post incomplete" }
        } on-error={
            # Existing confirmations expire according to the agent policy.
            # Do not delete, repair, or adopt ambiguous guard routes here.
        }
    }
}

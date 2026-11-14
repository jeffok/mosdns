# DISABLED DESIGN DRAFT: no scheduler is created; do not install on production.
# Requires the yet-to-be-implemented /guardgc/next and /guardgc/ack contract,
# a single serialized ROS writer shared with guard creation, persistent journal,
# controlled IPv4 clients, full conntrack, no FastTrack/HW-offload/NOTRACK, and
# tested firewall ordering. This script alone does not establish these facts.
# The two switches below deliberately require an audited source edit to enable.
/system script add name=bgp-prod-guardgc-draft policy=read,write,test comment="DISABLED BGP guard GC draft" source={
    :local enabled false
    :local mutationEnabled false
    :if (($enabled = true) && ([/system script job print count-only where script=[:jobname]] <= 1)) do={
        :do {
            :local fetched [/tool fetch url="http://192.168.88.222:9080/guardgc/next" src-address=192.168.88.254 http-max-redirect-count=0 duration=3s idle-timeout=2s output=user as-value]
            :if (($fetched->"status") != "finished") do={ :error "guard GC fetch incomplete" }
            :local op [:deserialize from=json value=($fetched->"data")]
            :if (($op->"version") != 1) do={ :error "unsupported guard GC protocol" }
            :local action ($op->"action")
            :if ($action != "idle") do={
                :if (($action != "install_barrier") && ($action != "observe_conntrack") && ($action != "observe_withdrawn") && ($action != "remove_guard") && ($action != "remove_barrier")) do={ :error "unknown guard GC action" }
                :local epoch ($op->"epoch")
                :local operationID ($op->"operation_id")
                :local generation ($op->"generation")
                :local parentID ""
                :if ($action = "observe_withdrawn") do={ :set parentID ($op->"parent_operation_id") }
                :foreach id in={ $epoch; $operationID; $generation } do={
                    :if (([:typeof $id] != "str") || ([:len $id] != 24) || (($id ~ "^[0-9a-f]{24}") = false)) do={ :error "invalid operation identity" }
                }
                :if (($action = "observe_withdrawn") && (([:typeof $parentID] != "str") || ([:len $parentID] != 24) || (($parentID ~ "^[0-9a-f]{24}") = false))) do={ :error "invalid parent operation identity" }
                :local rawPrefix ($op->"prefix")
                :local prefixType [:typeof $rawPrefix]
                :if (($prefixType != "str") && ($prefixType != "ip-prefix")) do={ :error "invalid prefix type" }
                :local prefix [:tostr $rawPrefix]
                :local slash [:find $prefix "/"]
                :if ($slash = nil) do={
                    :if ($prefixType != "ip-prefix") do={ :error "missing /32" }
                    :set prefix ($prefix . "/32")
                    :set slash [:find $prefix "/"]
                }
                :if ([:pick $prefix $slash [:len $prefix]] != "/32") do={ :error "invalid prefix mask" }
                :local host [:pick $prefix 0 $slash]
                :local ip [:toip $host]
                :if (([:typeof $ip] != "ip") || ([:tostr $ip] != $host)) do={ :error "noncanonical IPv4" }
                :if (($action != "observe_conntrack") && ($action != "observe_withdrawn") && ($mutationEnabled != true)) do={ :error "mutation gate is disabled" }

                # Both anchors must be enabled passthrough rules before every
                # accept, fasttrack or jump in their chains. Any ambiguity stops.
                :local ensureBarriers do={
                    :local p $pfx
                    :local g $gen
                    :local create $createMissing
                    :local total [/ip firewall filter print count-only where comment~"^BGP-PROD-GC:"]
                    :if ($total > 192) do={ :error "barrier cap exceeded" }
                    # Never adopt/ignore another generation's barrier for the
                    # same prefix; it may block traffic after this op ends.
                    :foreach existing in=[/ip firewall filter find where comment~"^BGP-PROD-GC:"] do={
                        :local label [:tostr [/ip firewall filter get $existing comment]]
                        :if (([:find $label (":" . $p . ":")] != nil) && ([:find $label ("BGP-PROD-GC:" . $g . ":" . $p . ":")] != 0)) do={ :error "stale-generation barrier" }
                    }
                    :foreach chain in={ "forward"; "output" } do={
                        :local anchorName ("BGP-PROD-GC-ANCHOR:" . $chain)
                        :local anchors [/ip firewall filter find where comment=$anchorName]
                        :if ([:len $anchors] != 1) do={ :error "missing or duplicate barrier anchor" }
                        :local anchor ($anchors->0)
                        :if (([/ip firewall filter get $anchor chain] != $chain) || ([/ip firewall filter get $anchor action] != "passthrough") || ([/ip firewall filter get $anchor disabled] != false)) do={ :error "invalid barrier anchor" }
                        :foreach state in={ "new"; "invalid"; "untracked" } do={
                            :local label ("BGP-PROD-GC:" . $g . ":" . $p . ":" . $chain . ":" . $state)
                            :local found [/ip firewall filter find where comment=$label]
                            :if ([:len $found] > 1) do={ :error "duplicate barrier owner" }
                            :if ([:len $found] = 0) do={
                                :if (($create != true) || ($total >= 192)) do={ :error "barrier missing or cap exceeded" }
                                /ip firewall filter add chain=$chain action=drop dst-address=$p connection-state=$state comment=$label place-before=$anchor
                                :set total ($total + 1)
                                :set found [/ip firewall filter find where comment=$label]
                            }
                            :if ([:len $found] != 1) do={ :error "barrier add/readback failed" }
                            :local rule ($found->0)
                            :local dst [:tostr [/ip firewall filter get $rule dst-address]]
                            :if ([:find $dst "/"] = nil) do={ :set dst ($dst . "/32") }
                            :if (([/ip firewall filter get $rule chain] != $chain) || ([/ip firewall filter get $rule action] != "drop") || ([/ip firewall filter get $rule disabled] != false) || ($dst != $p) || ([:tostr [/ip firewall filter get $rule connection-state]] != $state)) do={ :error "invalid barrier readback" }
                            # Check the barrier itself occurs before its anchor.
                            :local list [/ip firewall filter print as-value where chain=$chain]
                            :local ruleSeen false
                            :local anchorSeen false
                            :foreach row in=$list do={
                                :local rowID [:tostr ($row->".id")]
                                :if ($rowID = [:tostr $rule]) do={ :set ruleSeen true }
                                :if ($rowID = [:tostr $anchor]) do={
                                    :if ($ruleSeen != true) do={ :error "barrier follows anchor" }
                                    :set anchorSeen true
                                }
                                :if ($anchorSeen != true) do={
                                    :local beforeAction [:tostr ($row->"action")]
                                    :if (($beforeAction = "accept") || ($beforeAction = "fasttrack-connection") || ($beforeAction = "jump")) do={ :error "firewall bypass before barrier anchor" }
                                }
                            }
                            :if ($anchorSeen != true) do={ :error "anchor missing from ordered chain" }
                        }
                    }
                    :return true
                }

                # Inspect original and reply endpoints. Missing fields make the
                # entire observation unknown; no mutation is permitted. ROS
                # version-specific tuple formats must be tested before use.
                :local countConnections do={
                    :local target $hostIP
                    :local count 0
                    :local entries [/ip firewall connection print detail as-value]
                    :foreach entry in=$entries do={
                        :local matched false
                        :foreach field in={ "src-address"; "dst-address"; "reply-src-address"; "reply-dst-address" } do={
                            :local value ($entry->$field)
                            :if ([:typeof $value] = "nothing") do={ :error "incomplete conntrack tuple" }
                            :local text [:tostr $value]
                            :if ([:len $text] = 0) do={ :error "empty conntrack tuple" }
                            :local sep [:find $text ":"]
                            :if ($sep != nil) do={ :set text [:pick $text 0 $sep] }
                            :if ($text = $target) do={ :set matched true }
                        }
                        :if ($matched = true) do={ :set count ($count + 1) }
                    }
                    :return $count
                }

                # Verify the exact owned guard and active OVPN BGP path. Main
                # must have no other BGP peer capable of originating this route;
                # route ownership cannot be inferred from these attributes alone.
                :local verifyLiveRoute do={
                    :local p $pfx
                    :local g $gen
                    :local label ("BGP-PROD-BH:" . $g . ":" . $p)
                    :local owned [/ip route find where comment=$label]
                    :local allGuards [/ip route find where comment~"^BGP-PROD-BH:" and dst-address=$p]
                    :local good [/ip route find where comment=$label and routing-table="main" and dst-address=$p and static and blackhole and !disabled and distance=240]
                    :if (([:len $owned] != 1) || ([:len $allGuards] != 1) || ([:len $good] != 1) || ([:tostr ($good->0)] != [:tostr ($owned->0)])) do={ :error "live guard proof incomplete" }
                    :local active [/ip route find where routing-table="main" and dst-address=$p and bgp and active]
                    :local allActive [/ip route find where routing-table="main" and dst-address=$p and active]
                    :if (([:len $active] = 0) || ([:len $active] != [:len $allActive])) do={ :error "live BGP or active route proof incomplete" }
                    :foreach id in=$active do={
                        :if (([:tostr [/ip route get $id gateway]] != "ovpn-szof") || ([:tostr [/ip route get $id immediate-gw]] != "ovpn-szof") || ([/ip route get $id distance] != 20)) do={ :error "unexpected BGP egress" }
                    }
                    :return true
                }
                :local verifyGuard do={
                    :local p $pfx
                    :local label ("BGP-PROD-BH:" . $gen . ":" . $p)
                    :local owned [/ip route find where comment=$label]
                    :local allGuards [/ip route find where comment~"^BGP-PROD-BH:" and dst-address=$p]
                    :local good [/ip route find where comment=$label and routing-table="main" and dst-address=$p and static and blackhole and !disabled and distance=240]
                    :if (([:len $owned] != 1) || ([:len $allGuards] != 1) || ([:len $good] != 1) || ([:tostr ($good->0)] != [:tostr ($owned->0)])) do={ :error "guard proof incomplete" }
                    :return true
                }
                :local obs {"exact_owner"=false; "barrier_verified"=false; "snapshot_complete"=false; "connections"=-1; "guard_present"=false; "bgp_active"=false; "guard_absent"=false; "bgp_absent"=false; "barrier_absent"=false; "rechecked"=false}
                :local createMissing false
                :if (($action = "install_barrier") || ($action = "remove_barrier")) do={ :set createMissing true }
                :local barrierOK [$ensureBarriers pfx=$prefix gen=$generation createMissing=$createMissing]
                :if ($barrierOK != true) do={ :error "barrier verification failed" }
                :set ($obs->"barrier_verified") true
                :if (($action = "install_barrier") || ($action = "observe_conntrack")) do={
                    :local live [$verifyLiveRoute pfx=$prefix gen=$generation]
                    :if ($live != true) do={ :error "live route verification failed" }
                    :set ($obs->"guard_present") true
                    :set ($obs->"bgp_active") true
                }
                :if ($action = "observe_withdrawn") do={
                    :local guarded [$verifyGuard pfx=$prefix gen=$generation]
                    :if ($guarded != true) do={ :error "guard verification failed" }
                    :if ([:len [/ip route find where routing-table="main" and bgp and dst-address=$prefix]] != 0) do={ :error "BGP path still present" }
                    :set ($obs->"guard_present") true
                    :set ($obs->"bgp_absent") true
                }
                :set ($obs->"exact_owner") true
                :if ($action != "install_barrier") do={
                    :local connections [$countConnections hostIP=$host]
                    :set ($obs->"connections") $connections
                    :set ($obs->"snapshot_complete") true
                    :if ((($action = "remove_guard") || ($action = "remove_barrier")) && ($connections != 0)) do={ :error "active connection prevents removal" }
                    :if ($action = "observe_conntrack") do={ :local liveAgain [$verifyLiveRoute pfx=$prefix gen=$generation]; :if ($liveAgain != true) do={ :error "live route changed during snapshot" } }
                    :if ($action = "observe_withdrawn") do={
                        :local guardAgain [$verifyGuard pfx=$prefix gen=$generation]
                        :if (($guardAgain != true) || ([:len [/ip route find where routing-table="main" and bgp and dst-address=$prefix]] != 0)) do={ :error "withdrawn route changed during snapshot" }
                    }
                }

                :if (($action = "remove_guard") || ($action = "remove_barrier")) do={
                    # Never remove a guard while any BGP path for the prefix
                    # exists, including an inactive one.
                    :local bgp [/ip route find where routing-table="main" and bgp and dst-address=$prefix]
                    :if ([:len $bgp] != 0) do={ :error "BGP route still present" }
                    :set ($obs->"bgp_absent") true
                    :local guardLabel ("BGP-PROD-BH:" . $generation . ":" . $prefix)
                    :local owned [/ip route find where comment=$guardLabel]
                    :local allPrefixGuards [/ip route find where comment~"^BGP-PROD-BH:" and dst-address=$prefix]
                    :if (([:len $owned] > 1) || ([:len $allPrefixGuards] != [:len $owned])) do={ :error "duplicate or foreign-generation guard" }
                    :if ($action = "remove_guard") do={
                        # Without a separate durable ROS-side deletion receipt,
                        # an already-absent guard cannot prove exact ownership.
                        :if ([:len $owned] != 1) do={ :error "absent guard requires manual reconciliation" }
                        :if ([:len $owned] = 1) do={
                            :local good [/ip route find where comment=$guardLabel and routing-table="main" and dst-address=$prefix and static and blackhole and !disabled and distance=240]
                            :if (([:len $good] != 1) || ([:tostr ($good->0)] != [:tostr ($owned->0)])) do={ :error "guard ownership/readback mismatch" }
                            /ip route remove ($owned->0)
                        }
                    }
                    :if (([:len [/ip route find where comment=$guardLabel]] != 0) || ([:len [/ip route find where comment~"^BGP-PROD-BH:" and dst-address=$prefix]] != 0)) do={ :error "guard still present" }
                    :set ($obs->"guard_absent") true
                }
                :if ($action = "remove_barrier") do={
                    # Agent must durably freeze the prefix for the whole op;
                    # no newer generation may become answerable until ACK.
                    :foreach chain in={ "forward"; "output" } do={
                        :foreach state in={ "new"; "invalid"; "untracked" } do={
                            :local label ("BGP-PROD-GC:" . $generation . ":" . $prefix . ":" . $chain . ":" . $state)
                            :local found [/ip firewall filter find where comment=$label]
                            :if ([:len $found] != 1) do={ :error "barrier ownership changed" }
                            /ip firewall filter remove ($found->0)
                        }
                    }
                    :foreach chain in={ "forward"; "output" } do={
                        :foreach state in={ "new"; "invalid"; "untracked" } do={
                            :local label ("BGP-PROD-GC:" . $generation . ":" . $prefix . ":" . $chain . ":" . $state)
                            :if ([:len [/ip firewall filter find where comment=$label]] != 0) do={ :error "barrier removal readback failed" }
                        }
                    }
                    :local afterConnections [$countConnections hostIP=$host]
                    :if (($afterConnections != 0) || ([:len [/ip route find where routing-table="main" and bgp and dst-address=$prefix]] != 0) || ([:len [/ip route find where comment~"^BGP-PROD-BH:" and dst-address=$prefix]] != 0)) do={ :error "post-removal recheck failed" }
                    :set ($obs->"connections") $afterConnections
                    :set ($obs->"barrier_absent") true
                    :set ($obs->"rechecked") true
                }
                :local payload [:serialize to=json options=json.no-string-conversion value={"version"=1;"epoch"=$epoch;"operation_id"=$operationID;"parent_operation_id"=$parentID;"generation"=$generation;"prefix"=$prefix;"action"=$action;"result"="ok";"observation"=$obs}]
                :local posted [/tool fetch url="http://192.168.88.222:9080/guardgc/ack" src-address=192.168.88.254 http-method=post http-header-field="Content-Type:application/json" http-data=$payload http-max-redirect-count=0 duration=3s idle-timeout=2s output=none as-value]
                :if (($posted->"status") != "finished") do={ :error "guard GC acknowledgement incomplete" }
            }
        } on-error={
            # Unknown or stale evidence does not authorize a deletion. There
            # is no local cleanup; durable agent intent must be reconciled.
        }
    }
}

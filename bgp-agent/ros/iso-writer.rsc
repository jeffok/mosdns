# GENERATED ISOLATED TEST ONLY: no household DNS or main-table mutations.
# DISABLED UNVERIFIED SINGLE-WRITER DRAFT: no scheduler is created; do not install on production.
# Requires the yet-to-be-implemented /guardwriter/next and /guardwriter/ack contract,
# a single serialized ROS writer shared with guard creation, persistent journal,
# controlled IPv4 clients, full conntrack, no FastTrack/HW-offload/NOTRACK, and
# tested firewall ordering. This script alone does not establish these facts.
# The two switches below deliberately require an audited source edit to enable.
/system script add name=szhome-mosdns-bgp-iso-writer policy=read,write,test comment="mosdns: guarded writer" source={
    :local enabled false
    :local mutationEnabled false
    :if (($enabled = true) && ([/system script job print count-only where script=[:jobname]] <= 1)) do={
        # Recovery runs in this same job and keeps it occupied across all
        # attestation pages. Administrators must not concurrently modify the
        # managed objects; RouterOS cannot fence an unrelated manual session.
        :do {
            :local fetched [/tool fetch url="http://192.168.88.222:9080/guardrecovery/next?page=0" src-address=192.168.88.254 http-max-redirect-count=0 duration=3s idle-timeout=2s output=user as-value]
            :if (($fetched->"status") != "finished") do={ :error "recovery fetch incomplete" }
            :local body [:tostr ($fetched->"data")]
            :if ([:len $body] > 0) do={
                :local request [:deserialize from=json options=json.no-string-conversion value=$body]
                :local challenge ($request->"challenge")
                :local recoveryEpoch ($request->"epoch")
                :local records ($request->"records")
                :local totalRequestPages ($request->"total_pages")
                :if ((($request->"version") != 1) || (($request->"page") != 0) || ([:typeof $records] != "array") || ([:len $records] > 128) || ([:typeof $totalRequestPages] != "num") || ($totalRequestPages < 1) || ($totalRequestPages > 4)) do={ :error "invalid recovery request" }
                :foreach ident in={ $challenge; $recoveryEpoch } do={
                    :if (([:typeof $ident] != "str") || ([:len $ident] != 24) || (($ident ~ "^[0-9a-f]{24}") = false)) do={ :error "invalid recovery identity" }
                }
                :if ($totalRequestPages > 1) do={
                    :for page from=1 to=($totalRequestPages - 1) do={
                        :local url ("http://192.168.88.222:9080/guardrecovery/next?page=" . $page)
                        :local next [/tool fetch url=$url src-address=192.168.88.254 http-max-redirect-count=0 duration=3s idle-timeout=2s output=user as-value]
                        :if (($next->"status") != "finished") do={ :error "recovery page fetch incomplete" }
                        :local part [:deserialize from=json options=json.no-string-conversion value=($next->"data")]
                        :local partRecords ($part->"records")
                        :if ((($part->"version") != 1) || (($part->"challenge") != $challenge) || (($part->"epoch") != $recoveryEpoch) || (($part->"page") != $page) || (($part->"total_pages") != $totalRequestPages) || ([:typeof $partRecords] != "array") || ([:len $partRecords] > 128)) do={ :error "mismatched recovery page" }
                        :set records ($records, $partRecords)
                    }
                }
                :if ([:len $records] > 512) do={ :error "recovery record cap exceeded" }
                :if ([/system script job print count-only where script=[:jobname]] != 1) do={ :error "writer overlap" }
                :foreach job in=[/system script job print as-value] do={
                    :local name ([:tostr ($job->"script")] . " " . [:tostr ($job->"trace")])
                    :if (($name ~ "bgp|mosdns") && (($job->"script") != [:jobname])) do={ :error "old writer job still running" }
                }
                :foreach sched in=[/system scheduler print as-value] do={
                    :local name [:tostr ($sched->"name")]
                    :if (($name ~ "bgp|mosdns") && ($name != "szhome-mosdns-bgp-iso-writer") && (($sched->"disabled") != true)) do={ :error "old writer scheduler enabled" }
                }
                # A GUARD_REMOVED recovery may have stopped partway through
                # removing barriers. Restore all six before observing any
                # connections; a lost ACK cannot authorize an unguarded scan.
                :local preGuards [/ip route print detail as-value where comment~"^BGP-ISO-BH:"]
                :local preGuardCount [:toarray ""]
                :local preGuardPrefix [:toarray ""]
                :local preGuardLabels [:toarray ""]
                :foreach route in=$preGuards do={
                    :local p [:tostr ($route->"dst-address")]
                    :if ([:typeof [:find $p "/"]] = "nil") do={ :set p ($p . "/32") }
                    :if ([:typeof ($preGuardCount->$p)] = "nothing") do={ :set ($preGuardCount->$p) 0 }
                    :set ($preGuardCount->$p) (($preGuardCount->$p) + 1)
                    :local label [:tostr ($route->"comment")]
                    :if ([:typeof ($preGuardLabels->$label)] = "nothing") do={ :set ($preGuardLabels->$label) 0 }
                    :set ($preGuardLabels->$label) (($preGuardLabels->$label) + 1)
                    :if ((($route->"static") = true) && (($route->"blackhole") = true) && (($route->"disabled") != true) && ([:tostr ($route->"routing-table")] = "rt-bgp-test") && (($route->"distance") = 240)) do={ :set ($preGuardPrefix->[:tostr ($route->"comment")]) $p }
                }
                :foreach record in=$records do={
                    :local p [:tostr ($record->"prefix")]
                    :local g ($record->"generation")
                    :local slash [:find $p "/"]
                    :if (([:typeof $g] != "str") || ([:len $g] != 24) || (($g ~ "^[0-9a-f]{24}") = false) || ([:typeof $slash] = "nil") || ([:pick $p $slash [:len $p]] != "/32")) do={ :error "invalid recovery identity" }
                    :local host [:pick $p 0 $slash]
                    :if (([:typeof [:toip $host]] != "ip") || ([:tostr [:toip $host]] != $host)) do={ :error "invalid recovery prefix" }
                    :if (($host = "183.56.180.152") || ($host = "8.8.8.8") || ($host = "1.1.1.1")) do={ :error "protected recovery prefix" }
                    :local phase ($record->"phase")
                    :if (($phase = "ACTIVE") || ($phase = "GUARDED") || ($phase = "DRAINING") || ($phase = "BARRIER")) do={
                        :if ($mutationEnabled != true) do={ :error "recovery mutation gate disabled" }
                        :local label ("BGP-ISO-BH:" . $g . ":" . $p)
                        :local n ($preGuardCount->$p)
                        :if ([:typeof $n] = "nothing") do={ :set n 0 }
                        :if ($n > 1) do={ :error "recovery guard conflict" }
                        :if ($n = 0) do={
                            :if ([:typeof ($preGuardLabels->$label)] != "nothing") do={ :error "recovery guard label already bound elsewhere" }
                            /ip route add dst-address=$p routing-table=rt-bgp-test blackhole distance=240 comment=$label
                            :local mine [/ip route find where comment=$label]
                            :local good [/ip route find where comment=$label and routing-table="rt-bgp-test" and dst-address=$p and static and blackhole and !disabled and distance=240]
                            :if (([:len $mine] != 1) || ([:len $good] != 1) || ([:tostr ($mine->0)] != [:tostr ($good->0)])) do={ :error "recovery guard readback failed" }
                        } else={
                            :if ((($preGuardPrefix->$label) != $p) || (($preGuardLabels->$label) != 1)) do={ :error "recovery guard owner conflict" }
                        }
                    }
                    :if ((($record->"phase") = "GUARD_REMOVED") || (($record->"phase") = "BARRIER") || (($record->"phase") = "WITHDRAWN")) do={
                        :if ($mutationEnabled != true) do={ :error "recovery mutation gate disabled" }
                        :local total [/ip firewall filter print count-only where comment~"^BGP-ISO-GC:"]
                        :foreach existing in=[/ip firewall filter find where comment~"^BGP-ISO-GC:"] do={
                            :local label [:tostr [/ip firewall filter get $existing comment]]
                            :if (([:typeof [:find $label (":" . $p . ":")]] != "nil") && ([:find $label ("BGP-ISO-GC:" . $g . ":" . $p . ":")] != 0)) do={ :error "stale recovery barrier generation" }
                        }
                        :foreach chain in={ "forward"; "output" } do={
                            :local anchors [/ip firewall filter find where comment=("BGP-ISO-GC-ANCHOR:" . $chain)]
                            :if ([:len $anchors] != 1) do={ :error "recovery barrier anchor missing" }
                            :local anchor ($anchors->0)
                            :if (([/ip firewall filter get $anchor chain] != $chain) || ([/ip firewall filter get $anchor action] != "passthrough") || ([/ip firewall filter get $anchor disabled] != false)) do={ :error "invalid recovery anchor" }
                            :foreach state in={ "new"; "invalid"; "untracked" } do={
                                :local label ("BGP-ISO-GC:" . $g . ":" . $p . ":" . $chain . ":" . $state)
                                :local found [/ip firewall filter find where comment=$label]
                                :if ([:len $found] > 1) do={ :error "duplicate recovery barrier" }
                                :if ([:len $found] = 0) do={
                                    :if ($total >= 192) do={ :error "recovery barrier cap exceeded" }
                                    /ip firewall filter add chain=$chain action=drop src-address=192.168.88.222/32 dst-address=$p connection-state=$state comment=$label place-before=$anchor
                                    :set total ($total + 1)
                                    :set found [/ip firewall filter find where comment=$label]
                                }
                                :if ([:len $found] != 1) do={ :error "recovery barrier readback failed" }
                                :local rule ($found->0)
                                :local dst [:tostr [/ip firewall filter get $rule dst-address]]
                                :if ([:typeof [:find $dst "/"]] = "nil") do={ :set dst ($dst . "/32") }
                                :if (([/ip firewall filter get $rule chain] != $chain) || ([/ip firewall filter get $rule action] != "drop") || ([/ip firewall filter get $rule disabled] != false) || (([:tostr [/ip firewall filter get $rule src-address]] != "192.168.88.222/32") && ([:tostr [/ip firewall filter get $rule src-address]] != "192.168.88.222")) || ($dst != $p) || ([:tostr [/ip firewall filter get $rule connection-state]] != $state)) do={ :error "invalid recovery barrier" }
                                :local rules [/ip firewall filter print as-value where chain=$chain]
                                :local ruleSeen false
                                :local anchorSeen false
                                :foreach row in=$rules do={
                                    :local rowID [:tostr ($row->".id")]
                                    :if ($rowID = [:tostr $rule]) do={ :set ruleSeen true }
                                    :if ($rowID = [:tostr $anchor]) do={ :if ($ruleSeen != true) do={ :error "recovery barrier follows anchor" }; :set anchorSeen true }
                                    :if ($anchorSeen != true) do={
                                        :local action [:tostr ($row->"action")]
                                        :if (($action = "accept") || ($action = "fasttrack-connection") || ($action = "jump")) do={ :error "recovery barrier bypass" }
                                    }
                                }
                                :if ($anchorSeen != true) do={ :error "recovery anchor not in ordered chain" }
                            }
                        }
                    }
                }
                :local guards [/ip route print detail as-value where comment~"^BGP-ISO-BH:"]
                :local bgpRoutes [/ip route print detail as-value where routing-table="rt-bgp-test" and bgp]
                :local barriers [/ip firewall filter print as-value where comment~"^BGP-ISO-GC:"]
                :local expectedGuard [:toarray ""]
                :local expectedPrefix [:toarray ""]
                :local expectedBarrier [:toarray ""]
                :local barrierChain [:toarray ""]
                :local barrierState [:toarray ""]
                :local barrierPrefix [:toarray ""]
                :local inspectConnections false
                :foreach record in=$records do={
                    :local prefix [:tostr ($record->"prefix")]
                    :local gen ($record->"generation")
                    :if (([:typeof $gen] != "str") || ([:len $gen] != 24) || (($gen ~ "^[0-9a-f]{24}") = false) || (($expectedPrefix->$prefix) = true)) do={ :error "invalid recovery record" }
                    :set ($expectedPrefix->$prefix) true
                    :if ((($record->"phase") = "WITHDRAWN") || (($record->"phase") = "GUARD_REMOVED") || ((($record->"phase") = "BARRIER") && (($record->"pending_action") = "withdraw_bgp"))) do={ :set inspectConnections true }
                    :set ($expectedGuard->("BGP-ISO-BH:" . $gen . ":" . $prefix)) true
                    :foreach chain in={ "forward"; "output" } do={
                        :foreach state in={ "new"; "invalid"; "untracked" } do={
                            :local label ("BGP-ISO-GC:" . $gen . ":" . $prefix . ":" . $chain . ":" . $state)
                            :set ($expectedBarrier->$label) true
                            :set ($barrierChain->$label) $chain
                            :set ($barrierState->$label) $state
                            :set ($barrierPrefix->$label) $prefix
                        }
                    }
                }
                # Build prefix indexes in one pass over each bulk snapshot.
                :local unexpected 0
                :local guardCounts [:toarray ""]
                :local guardGood [:toarray ""]
                :local guardLabelCounts [:toarray ""]
                :local bgpCounts [:toarray ""]
                :local bgpGood [:toarray ""]
                :local barrierCounts [:toarray ""]
                :local barrierGood [:toarray ""]
                :local seenBarriers [:toarray ""]
                :local connectionCounts [:toarray ""]
                :foreach record in=$records do={
                    :local prefix [:tostr ($record->"prefix")]
                    :set ($guardCounts->$prefix) 0
                    :set ($bgpCounts->$prefix) 0
                    :set ($barrierCounts->$prefix) 0
                    :set ($barrierGood->$prefix) 0
                    :local slash [:find $prefix "/"]
                    :if ([:typeof $slash] = "nil") do={ :error "invalid recovery prefix" }
                    :set ($connectionCounts->[:pick $prefix 0 $slash]) 0
                }
                :if ($inspectConnections = true) do={
                    # One full snapshot; an incomplete tuple cannot authorize
                    # recovery or removal. Count every exact IPv4 endpoint.
                    :local connections [/ip firewall connection print detail as-value]
                    :foreach connection in=$connections do={
                        :foreach field in={ "src-address"; "dst-address"; "reply-src-address"; "reply-dst-address" } do={
                            :local value ($connection->$field)
                            :if (([:typeof $value] = "nothing") || ([:len [:tostr $value]] = 0)) do={ :error "incomplete recovery connection tuple" }
                            :local host [:tostr $value]
                            :local sep [:find $host ":"]
                            :if ([:typeof $sep] != "nil") do={ :set host [:pick $host 0 $sep] }
                            :if ([:typeof [:toip $host]] != "ip") do={ :error "invalid recovery connection tuple" }
                            :if ([:typeof ($connectionCounts->$host)] != "nothing") do={ :set ($connectionCounts->$host) (($connectionCounts->$host) + 1) }
                        }
                    }
                }
                :foreach route in=$guards do={
                    :local label [:tostr ($route->"comment")]
                    :if ([:typeof ($guardLabelCounts->$label)] = "nothing") do={ :set ($guardLabelCounts->$label) 0 }
                    :set ($guardLabelCounts->$label) (($guardLabelCounts->$label) + 1)
                    :local prefix [:tostr ($route->"dst-address")]
                    :if ([:typeof [:find $prefix "/"]] = "nil") do={ :set prefix ($prefix . "/32") }
                    :if ((($expectedGuard->$label) != true) || (($expectedPrefix->$prefix) != true)) do={
                        :set unexpected ($unexpected + 1)
                    } else={
                        :set ($guardCounts->$prefix) (($guardCounts->$prefix) + 1)
                        :if ((($route->"static") = true) && (($route->"blackhole") = true) && (($route->"disabled") != true) && ([:tostr ($route->"routing-table")] = "rt-bgp-test") && (($route->"distance") = 240)) do={ :set ($guardGood->$label) $prefix }
                    }
                }
                :foreach route in=$bgpRoutes do={
                    :local prefix [:tostr ($route->"dst-address")]
                    :if ([:typeof [:find $prefix "/"]] = "nil") do={ :set prefix ($prefix . "/32") }
                    :if (($expectedPrefix->$prefix) != true) do={
                        :set unexpected ($unexpected + 1)
                    } else={
                        :set ($bgpCounts->$prefix) (($bgpCounts->$prefix) + 1)
                        :if ((($route->"active") = true) && ([:tostr ($route->"gateway")] = "ovpn-szof") && ([:tostr ($route->"immediate-gw")] = "ovpn-szof") && (($route->"distance") = 20)) do={ :set ($bgpGood->$prefix) true }
                    }
                }
                :foreach barrier in=$barriers do={
                    :local label [:tostr ($barrier->"comment")]
                    :if (($expectedBarrier->$label) != true) do={
                        :set unexpected ($unexpected + 1)
                    } else={
                        :local prefix ($barrierPrefix->$label)
                        :set ($barrierCounts->$prefix) (($barrierCounts->$prefix) + 1)
                        :local dst [:tostr ($barrier->"dst-address")]
                        :if ([:typeof [:find $dst "/"]] = "nil") do={ :set dst ($dst . "/32") }
                        :if ((($seenBarriers->$label) != true) && ([:tostr ($barrier->"chain")] = ($barrierChain->$label)) && ([:tostr ($barrier->"connection-state")] = ($barrierState->$label)) && ($dst = $prefix) && ([:tostr ($barrier->"action")] = "drop") && (($barrier->"disabled") != true) && (([:tostr ($barrier->"src-address")] = "192.168.88.222/32") || ([:tostr ($barrier->"src-address")] = "192.168.88.222"))) do={
                            :set ($barrierGood->$prefix) (($barrierGood->$prefix) + 1)
                            :set ($seenBarriers->$label) true
                        }
                    }
                }
                :local observations [:toarray ""]
                :foreach record in=$records do={
                    :local prefix [:tostr ($record->"prefix")]
                    :local gen ($record->"generation")
                    :local guardLabel ("BGP-ISO-BH:" . $gen . ":" . $prefix)
                    :local host [:pick $prefix 0 [:find $prefix "/"]]
                    :set ($observations->[:len $observations]) {"prefix"=$prefix;"generation"=$gen;"pending_id"=($record->"pending_id");"guard_present"=(($guardCounts->$prefix) > 0);"guard_owned"=((($guardCounts->$prefix) = 1) && (($guardLabelCounts->$guardLabel) = 1) && (($guardGood->$guardLabel) = $prefix));"bgp_present"=(($bgpCounts->$prefix) > 0);"bgp_verified"=((($bgpCounts->$prefix) = 1) && (($bgpGood->$prefix) = true));"barrier_present"=(($barrierCounts->$prefix) > 0);"barrier_owned"=((($barrierCounts->$prefix) = 6) && (($barrierGood->$prefix) = 6));"connections_complete"=$inspectConnections;"no_connections"=(($inspectConnections = true) && (($connectionCounts->$host) = 0))}
                }
                :local count [:len $observations]
                :local pages (($count + 7) / 8)
                :if ($pages = 0) do={ :set pages 1 }
                :for page from=0 to=($pages - 1) do={
                    :if ([/system script job print count-only where script=[:jobname]] != 1) do={ :error "writer overlap during recovery" }
                    :local items [:pick $observations ($page * 8) (($page + 1) * 8)]
                    :local payload [:serialize to=json options=json.no-string-conversion value={"version"=1;"challenge"=$challenge;"epoch"=$recoveryEpoch;"sequence"=$page;"total_pages"=$pages;"objects_complete"=true;"old_jobs_quiescent"=true;"writer_fenced"=true;"unexpected_objects"=$unexpected;"observations"=$items}]
                    :local posted [/tool fetch url="http://192.168.88.222:9080/guardrecovery/attest" src-address=192.168.88.254 http-method=post http-header-field="Content-Type:application/json" http-data=$payload http-max-redirect-count=0 duration=60s idle-timeout=55s output=none as-value]
                    :if (($posted->"status") != "finished") do={ :error "recovery attestation incomplete" }
                }
            }
        } on-error={
            # Recovery remains frozen on any uncertain or incomplete snapshot.
        }
        # Claim up to eight independent create/confirm operations in one
        # durable batch. A failed item and all later unexecuted items are
        # returned blocked; no failure is ever reported as successful.
        :local didBatch false
        :onerror batchErr in={
            :local batchStarted [:timestamp]
            :local fetched [/tool fetch url="http://192.168.88.222:9080/guardwriter/batch-next" src-address=192.168.88.254 http-max-redirect-count=0 duration=3s idle-timeout=2s output=user as-value]
            :if (($fetched->"status") != "finished") do={ :error "batch fetch incomplete" }
            :local batchNextElapsed ([:timestamp] - $batchStarted)
            :local body [:tostr ($fetched->"data")]
            :if ([:len $body] > 0) do={
                :local batch [:deserialize from=json options=json.no-string-conversion value=$body]
                :local batchID ($batch->"batch_id")
                :local ops ($batch->"operations")
                :if ((($batch->"version") != 1) || ([:typeof $batchID] != "str") || ([:len $batchID] != 24) || (($batchID ~ "^[0-9a-f]{24}") = false) || ([:typeof $ops] != "array") || ([:len $ops] < 1) || ([:len $ops] > 8)) do={ :error "invalid writer batch" }
                :set didBatch true
                :local results [:toarray ""]
                :local stopped false
                :foreach op in=$ops do={
                    :local result "blocked"
                    :local proof {"exact_owner"=false;"guard_present"=false;"guard_absent"=false;"barrier_present"=false;"barrier_absent"=false;"bgp_active"=false;"bgp_gone"=false;"no_connections"=false;"rechecked"=false}
                    :if ($stopped != true) do={
                        :onerror itemErr in={
                            :local action ($op->"action")
                            :if ((($op->"version") != 1) || (($action != "create_guard") && ($action != "confirm_bgp"))) do={ :error "invalid batch action" }
                            :foreach ident in={ ($op->"epoch"); ($op->"operation_id"); ($op->"generation") } do={
                                :if (([:typeof $ident] != "str") || ([:len $ident] != 24) || (($ident ~ "^[0-9a-f]{24}") = false)) do={ :error "invalid batch identity" }
                            }
                            :local prefix [:tostr ($op->"prefix")]
                            :local slash [:find $prefix "/"]
                            :if (([:typeof $slash] = "nil") || ([:pick $prefix $slash [:len $prefix]] != "/32")) do={ :error "invalid batch prefix" }
                            :local host [:pick $prefix 0 $slash]
                            :if (([:typeof [:toip $host]] != "ip") || ([:tostr [:toip $host]] != $host)) do={ :error "noncanonical batch prefix" }
                            :if (($host = "183.56.180.152") || ($host = "8.8.8.8") || ($host = "1.1.1.1")) do={ :error "protected batch host" }
                            :set ($op->"prefix") $prefix
                            :local generation ($op->"generation")
                            :local label ("BGP-ISO-BH:" . $generation . ":" . $prefix)
                            :local owners [/ip route find where comment~"^BGP-ISO-BH:" and dst-address=$prefix]
                            :local mine [/ip route find where comment=$label]
                            :if (([:len $mine] > 1) || ([:len $owners] != [:len $mine])) do={ :error "batch guard ownership conflict" }
                            :if ([:len $mine] = 0) do={
                                :if (($action != "create_guard") || ($mutationEnabled != true)) do={ :error "batch guard absent or mutation disabled" }
                                :if ([:len [/ip route find where routing-table="rt-bgp-test" and dst-address=$prefix and bgp]] != 0) do={ :error "BGP existed before batch guard" }
                                /ip route add dst-address=$prefix routing-table=rt-bgp-test blackhole distance=240 comment=$label
                                :set mine [/ip route find where comment=$label]
                            }
                            :local good [/ip route find where comment=$label and routing-table="rt-bgp-test" and dst-address=$prefix and static and blackhole and !disabled and distance=240]
                            :if (([:len $mine] != 1) || ([:len $good] != 1) || ([:tostr ($mine->0)] != [:tostr ($good->0)])) do={ :error "batch guard readback failed" }
                            :foreach barrier in=[/ip firewall filter find where comment~"^BGP-ISO-GC:"] do={
                                :if ([:typeof [:find [:tostr [/ip firewall filter get $barrier comment]] (":" . $prefix . ":")]] != "nil") do={ :error "batch barrier exists" }
                            }
                            :set ($proof->"exact_owner") true
                            :set ($proof->"guard_present") true
                            :if ($action = "confirm_bgp") do={
                                :local active [/ip route find where routing-table="rt-bgp-test" and dst-address=$prefix and bgp and active]
                                :local allActive [/ip route find where routing-table="rt-bgp-test" and dst-address=$prefix and active]
                                :if (([:len $active] = 0) || ([:len $active] != [:len $allActive])) do={ :error "batch active BGP unavailable" }
                                :foreach id in=$active do={
                                    :if (([:tostr [/ip route get $id gateway]] != "ovpn-szof") || ([:tostr [/ip route get $id immediate-gw]] != "ovpn-szof") || ([/ip route get $id distance] != 20)) do={ :error "batch unexpected BGP egress" }
                                }
                                :set ($proof->"bgp_active") true
                            }
                            :set result "ok"
                        } do={
                            :local runs [/system scheduler get [find where name="szhome-mosdns-bgp-iso-writer"] run-count]
                            :if (($runs % 5) = 0) do={ :log warning ("BGP-ISO-PROFILE batch-item-error action=" . ($op->"action") . " prefix=" . ($op->"prefix") . " error=" . $itemErr) }
                            :set stopped true
                        }
                    }
                    :set ($results->[:len $results]) {"operation"=$op;"result"=$result;"proof"=$proof}
                }
                :local payload [:serialize to=json options=json.no-string-conversion value={"version"=1;"batch_id"=$batchID;"results"=$results}]
                :if ([:len $payload] > 3900) do={ :error "batch acknowledgement too large" }
                :local batchAckStarted [:timestamp]
                :local posted [/tool fetch url="http://192.168.88.222:9080/guardwriter/batch-ack" src-address=192.168.88.254 http-method=post http-header-field="Content-Type:application/json" http-data=$payload http-max-redirect-count=0 duration=3s idle-timeout=2s output=none as-value]
                :if (($posted->"status") != "finished") do={ :error "batch acknowledgement incomplete" }
                :local runs [/system scheduler get [find where name="szhome-mosdns-bgp-iso-writer"] run-count]
                :if ((([:timestamp] - $batchStarted) > 300ms) || (($runs % 5) = 0)) do={ :log warning ("BGP-ISO-PROFILE batch count=" . [:len $ops] . " next=" . $batchNextElapsed . " pre-ack=" . ($batchAckStarted - $batchStarted) . " ack=" . ([:timestamp] - $batchAckStarted) . " total=" . ([:timestamp] - $batchStarted)) }
            }
        } do={
            :local runs [/system scheduler get [find where name="szhome-mosdns-bgp-iso-writer"] run-count]
            :if (($runs % 5) = 0) do={ :log warning ("BGP-ISO-PROFILE batch-error=" . $batchErr) }
            # The agent retains the same batch for safe replay.
            :set didBatch true
        }
        :local writerContinue true
        :local writerCount 0
        :local writerStarted [:timestamp]
        :while (($didBatch = false) && ($writerContinue = true) && ($writerCount < 32) && (([:timestamp] - $writerStarted) < 750ms)) do={
        :set writerCount ($writerCount + 1)
        :onerror writerErr in={
            :local opStarted [:timestamp]
            :local fetched [/tool fetch url="http://192.168.88.222:9080/guardwriter/next" src-address=192.168.88.254 http-max-redirect-count=0 duration=3s idle-timeout=2s output=user as-value]
            :if (($fetched->"status") != "finished") do={ :error "guard GC fetch incomplete" }
            :local nextElapsed ([:timestamp] - $opStarted)
            :local body [:tostr ($fetched->"data")]
            :local op {"version"=1;"action"="idle"}
            :if ([:len $body] > 0) do={ :set op [:deserialize from=json options=json.no-string-conversion value=$body] }
            :if (($op->"version") != 1) do={ :error "unsupported guard GC protocol" }
            :local action ($op->"action")
            :if ($action = "idle") do={ :set writerContinue false }
            :if ($action != "idle") do={
                :if (($action != "create_guard") && ($action != "confirm_bgp") && ($action != "remove_orphan_guard") && ($action != "install_barrier") && ($action != "observe_conntrack") && ($action != "observe_withdrawn") && ($action != "remove_guard") && ($action != "remove_barrier")) do={ :error "unknown guard GC action" }
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
                :if ([:typeof $slash] = "nil") do={
                    :if ($prefixType != "ip-prefix") do={ :error "missing /32" }
                    :set prefix ($prefix . "/32")
                    :set slash [:find $prefix "/"]
                }
                :if ([:pick $prefix $slash [:len $prefix]] != "/32") do={ :error "invalid prefix mask" }
                :local host [:pick $prefix 0 $slash]
                :local ip [:toip $host]
                :if (([:typeof $ip] != "ip") || ([:tostr $ip] != $host)) do={ :error "noncanonical IPv4" }
                :if (($host = "183.56.180.152") || ($host = "8.8.8.8") || ($host = "1.1.1.1")) do={ :error "protected bootstrap host" }
                :if (($action != "confirm_bgp") && ($action != "observe_conntrack") && ($action != "observe_withdrawn") && ($mutationEnabled != true)) do={ :error "mutation gate is disabled" }

                :local obs {"exact_owner"=false; "barrier_verified"=false; "snapshot_complete"=false; "connections"=-1; "guard_present"=false; "bgp_active"=false; "guard_absent"=false; "bgp_absent"=false; "barrier_absent"=false; "rechecked"=false}
                :if (($action = "create_guard") || ($action = "confirm_bgp") || ($action = "remove_orphan_guard")) do={
                    :if ($action = "remove_orphan_guard") do={
                        # Agent must freeze this generation and attest that no
                        # BGP route was published or DNS answer sent. ROS then
                        # independently checks BGP/barrier absence and owner.
                        :local label ("BGP-ISO-BH:" . $generation . ":" . $prefix)
                        :if ([:len [/ip route find where routing-table="rt-bgp-test" and dst-address=$prefix and bgp]] != 0) do={ :error "orphan has BGP route" }
                        :foreach barrier in=[/ip firewall filter find where comment~"^BGP-ISO-GC:"] do={
                            :if ([:typeof [:find [:tostr [/ip firewall filter get $barrier comment]] (":" . $prefix . ":")]] != "nil") do={ :error "orphan has barrier" }
                        }
                        :local mine [/ip route find where comment=$label]
                        :local owners [/ip route find where comment~"^BGP-ISO-BH:" and dst-address=$prefix]
                        :if (([:len $mine] > 1) || ([:len $owners] != [:len $mine])) do={ :error "orphan guard ownership conflict" }
                        :if ([:len $mine] = 1) do={
                            :local good [/ip route find where comment=$label and routing-table="rt-bgp-test" and dst-address=$prefix and static and blackhole and !disabled and distance=240]
                            :if (([:len $good] != 1) || ([:tostr ($good->0)] != [:tostr ($mine->0)])) do={ :error "invalid orphan guard" }
                            /ip route remove ($mine->0)
                        }
                        :if (([:len [/ip route find where comment=$label]] != 0) || ([:len [/ip route find where comment~"^BGP-ISO-BH:" and dst-address=$prefix]] != 0) || ([:len [/ip route find where routing-table="rt-bgp-test" and dst-address=$prefix and bgp]] != 0)) do={ :error "orphan removal readback failed" }
                        :set ($obs->"exact_owner") true
                        :set ($obs->"guard_absent") true
                        :set ($obs->"bgp_absent") true
                    } else={
                    :local label ("BGP-ISO-BH:" . $generation . ":" . $prefix)
                    :local owners [/ip route find where comment~"^BGP-ISO-BH:" and dst-address=$prefix]
                    :local mine [/ip route find where comment=$label]
                    :if (([:len $mine] > 1) || ([:len $owners] != [:len $mine])) do={ :error "guard ownership conflict" }
                    :if ([:len $mine] = 0) do={
                        :if ($action != "create_guard") do={ :error "guard missing for BGP confirmation" }
                        :if ([:len [/ip route find where routing-table="rt-bgp-test" and dst-address=$prefix and bgp]] != 0) do={ :error "BGP existed before guard" }
                        /ip route add dst-address=$prefix routing-table=rt-bgp-test blackhole distance=240 comment=$label
                        :set mine [/ip route find where comment=$label]
                    }
                    :local good [/ip route find where comment=$label and routing-table="rt-bgp-test" and dst-address=$prefix and static and blackhole and !disabled and distance=240]
                    :if (([:len $mine] != 1) || ([:len $good] != 1) || ([:tostr ($mine->0)] != [:tostr ($good->0)])) do={ :error "guard readback failed" }
                    :foreach barrier in=[/ip firewall filter find where comment~"^BGP-ISO-GC:"] do={
                        :if ([:typeof [:find [:tostr [/ip firewall filter get $barrier comment]] (":" . $prefix . ":")]] != "nil") do={ :error "barrier exists for prefix" }
                    }
                    :set ($obs->"exact_owner") true
                    :set ($obs->"guard_present") true
                    :if ($action = "confirm_bgp") do={
                        :local active [/ip route find where routing-table="rt-bgp-test" and dst-address=$prefix and bgp and active]
                        :local allActive [/ip route find where routing-table="rt-bgp-test" and dst-address=$prefix and active]
                        :if (([:len $active] = 0) || ([:len $active] != [:len $allActive])) do={ :error "active BGP proof unavailable" }
                        :foreach id in=$active do={
                            :if (([:tostr [/ip route get $id gateway]] != "ovpn-szof") || ([:tostr [/ip route get $id immediate-gw]] != "ovpn-szof") || ([/ip route get $id distance] != 20)) do={ :error "unexpected BGP egress" }
                        }
                        :set ($obs->"bgp_active") true
                    }
                    }
                } else={
                # Both anchors must be enabled passthrough rules before every
                # accept, fasttrack or jump in their chains. Any ambiguity stops.
                :local ensureBarriers do={
                    :local p $pfx
                    :local g $gen
                    :local create $createMissing
                    :local total [/ip firewall filter print count-only where comment~"^BGP-ISO-GC:"]
                    :if ($total > 192) do={ :error "barrier cap exceeded" }
                    # Never adopt/ignore another generation's barrier for the
                    # same prefix; it may block traffic after this op ends.
                    :foreach existing in=[/ip firewall filter find where comment~"^BGP-ISO-GC:"] do={
                        :local label [:tostr [/ip firewall filter get $existing comment]]
                        :if (([:typeof [:find $label (":" . $p . ":")]] != "nil") && ([:find $label ("BGP-ISO-GC:" . $g . ":" . $p . ":")] != 0)) do={ :error "stale-generation barrier" }
                    }
                    :foreach chain in={ "forward"; "output" } do={
                        :local anchorName ("BGP-ISO-GC-ANCHOR:" . $chain)
                        :local anchors [/ip firewall filter find where comment=$anchorName]
                        :if ([:len $anchors] != 1) do={ :error "missing or duplicate barrier anchor" }
                        :local anchor ($anchors->0)
                        :if (([/ip firewall filter get $anchor chain] != $chain) || ([/ip firewall filter get $anchor action] != "passthrough") || ([/ip firewall filter get $anchor disabled] != false)) do={ :error "invalid barrier anchor" }
                        :foreach state in={ "new"; "invalid"; "untracked" } do={
                            :local label ("BGP-ISO-GC:" . $g . ":" . $p . ":" . $chain . ":" . $state)
                            :local found [/ip firewall filter find where comment=$label]
                            :if ([:len $found] > 1) do={ :error "duplicate barrier owner" }
                            :if ([:len $found] = 0) do={
                                :if (($create != true) || ($total >= 192)) do={ :error "barrier missing or cap exceeded" }
                                /ip firewall filter add chain=$chain action=drop src-address=192.168.88.222/32 dst-address=$p connection-state=$state comment=$label place-before=$anchor
                                :set total ($total + 1)
                                :set found [/ip firewall filter find where comment=$label]
                            }
                            :if ([:len $found] != 1) do={ :error "barrier add/readback failed" }
                            :local rule ($found->0)
                            :local dst [:tostr [/ip firewall filter get $rule dst-address]]
                            :if ([:typeof [:find $dst "/"]] = "nil") do={ :set dst ($dst . "/32") }
                            :if (([/ip firewall filter get $rule chain] != $chain) || ([/ip firewall filter get $rule action] != "drop") || ([/ip firewall filter get $rule disabled] != false) || (([:tostr [/ip firewall filter get $rule src-address]] != "192.168.88.222/32") && ([:tostr [/ip firewall filter get $rule src-address]] != "192.168.88.222")) || ($dst != $p) || ([:tostr [/ip firewall filter get $rule connection-state]] != $state)) do={ :error "invalid barrier readback" }
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
                            :if ([:typeof $sep] != "nil") do={ :set text [:pick $text 0 $sep] }
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
                    :local label ("BGP-ISO-BH:" . $g . ":" . $p)
                    :local owned [/ip route find where comment=$label]
                    :local allGuards [/ip route find where comment~"^BGP-ISO-BH:" and dst-address=$p]
                    :local good [/ip route find where comment=$label and routing-table="rt-bgp-test" and dst-address=$p and static and blackhole and !disabled and distance=240]
                    :if (([:len $owned] != 1) || ([:len $allGuards] != 1) || ([:len $good] != 1) || ([:tostr ($good->0)] != [:tostr ($owned->0)])) do={ :error "live guard proof incomplete" }
                    :local active [/ip route find where routing-table="rt-bgp-test" and dst-address=$p and bgp and active]
                    :local allActive [/ip route find where routing-table="rt-bgp-test" and dst-address=$p and active]
                    :if (([:len $active] = 0) || ([:len $active] != [:len $allActive])) do={ :error "live BGP or active route proof incomplete" }
                    :foreach id in=$active do={
                        :if (([:tostr [/ip route get $id gateway]] != "ovpn-szof") || ([:tostr [/ip route get $id immediate-gw]] != "ovpn-szof") || ([/ip route get $id distance] != 20)) do={ :error "unexpected BGP egress" }
                    }
                    :return true
                }
                :local verifyGuard do={
                    :local p $pfx
                    :local label ("BGP-ISO-BH:" . $gen . ":" . $p)
                    :local owned [/ip route find where comment=$label]
                    :local allGuards [/ip route find where comment~"^BGP-ISO-BH:" and dst-address=$p]
                    :local good [/ip route find where comment=$label and routing-table="rt-bgp-test" and dst-address=$p and static and blackhole and !disabled and distance=240]
                    :if (([:len $owned] != 1) || ([:len $allGuards] != 1) || ([:len $good] != 1) || ([:tostr ($good->0)] != [:tostr ($owned->0)])) do={ :error "guard proof incomplete" }
                    :return true
                }
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
                    :if ([:len [/ip route find where routing-table="rt-bgp-test" and bgp and dst-address=$prefix]] != 0) do={ :error "BGP path still present" }
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
                        :if (($guardAgain != true) || ([:len [/ip route find where routing-table="rt-bgp-test" and bgp and dst-address=$prefix]] != 0)) do={ :error "withdrawn route changed during snapshot" }
                    }
                }

                :if (($action = "remove_guard") || ($action = "remove_barrier")) do={
                    # Never remove a guard while any BGP path for the prefix
                    # exists, including an inactive one.
                    :local bgp [/ip route find where routing-table="rt-bgp-test" and bgp and dst-address=$prefix]
                    :if ([:len $bgp] != 0) do={ :error "BGP route still present" }
                    :set ($obs->"bgp_absent") true
                    :local guardLabel ("BGP-ISO-BH:" . $generation . ":" . $prefix)
                    :local owned [/ip route find where comment=$guardLabel]
                    :local allPrefixGuards [/ip route find where comment~"^BGP-ISO-BH:" and dst-address=$prefix]
                    :if (([:len $owned] > 1) || ([:len $allPrefixGuards] != [:len $owned])) do={ :error "duplicate or foreign-generation guard" }
                    :if ($action = "remove_guard") do={
                        # A previous attempt may have removed the guard before
                        # its ACK was lost. Replay is safe only after the same
                        # barrier, complete empty conntrack and BGP-absence
                        # checks above, and while the agent freezes this prefix.
                        :if ([:len $owned] = 1) do={
                            :local good [/ip route find where comment=$guardLabel and routing-table="rt-bgp-test" and dst-address=$prefix and static and blackhole and !disabled and distance=240]
                            :if (([:len $good] != 1) || ([:tostr ($good->0)] != [:tostr ($owned->0)])) do={ :error "guard ownership/readback mismatch" }
                            /ip route remove ($owned->0)
                        }
                    }
                    :if (([:len [/ip route find where comment=$guardLabel]] != 0) || ([:len [/ip route find where comment~"^BGP-ISO-BH:" and dst-address=$prefix]] != 0)) do={ :error "guard still present" }
                    :set ($obs->"guard_absent") true
                }
                :if ($action = "remove_barrier") do={
                    # Agent must durably freeze the prefix for the whole op;
                    # no newer generation may become answerable until ACK.
                    :foreach chain in={ "forward"; "output" } do={
                        :foreach state in={ "new"; "invalid"; "untracked" } do={
                            :local label ("BGP-ISO-GC:" . $generation . ":" . $prefix . ":" . $chain . ":" . $state)
                            :local found [/ip firewall filter find where comment=$label]
                            :if ([:len $found] != 1) do={ :error "barrier ownership changed" }
                            /ip firewall filter remove ($found->0)
                        }
                    }
                    :foreach chain in={ "forward"; "output" } do={
                        :foreach state in={ "new"; "invalid"; "untracked" } do={
                            :local label ("BGP-ISO-GC:" . $generation . ":" . $prefix . ":" . $chain . ":" . $state)
                            :if ([:len [/ip firewall filter find where comment=$label]] != 0) do={ :error "barrier removal readback failed" }
                        }
                    }
                    :local afterConnections [$countConnections hostIP=$host]
                    :if (($afterConnections != 0) || ([:len [/ip route find where routing-table="rt-bgp-test" and bgp and dst-address=$prefix]] != 0) || ([:len [/ip route find where comment~"^BGP-ISO-BH:" and dst-address=$prefix]] != 0)) do={ :error "post-removal recheck failed" }
                    :set ($obs->"connections") $afterConnections
                    :set ($obs->"barrier_absent") true
                    :set ($obs->"rechecked") true
                }
                }
                :local proof {"exact_owner"=($obs->"exact_owner");"guard_present"=($obs->"guard_present");"guard_absent"=($obs->"guard_absent");"barrier_present"=($obs->"barrier_verified");"barrier_absent"=($obs->"barrier_absent");"bgp_active"=($obs->"bgp_active");"bgp_gone"=($obs->"bgp_absent");"no_connections"=(($obs->"snapshot_complete") && (($obs->"connections") = 0));"rechecked"=($obs->"rechecked")}
                :if ($action = "remove_barrier") do={ :set ($proof->"barrier_present") false }
                :local payload [:serialize to=json options=json.no-string-conversion value={"version"=1;"epoch"=$epoch;"operation_id"=$operationID;"parent_operation_id"=$parentID;"generation"=$generation;"prefix"=$prefix;"action"=$action;"result"="ok";"proof"=$proof}]
                :local ackStarted [:timestamp]
                :local posted [/tool fetch url="http://192.168.88.222:9080/guardwriter/ack" src-address=192.168.88.254 http-method=post http-header-field="Content-Type:application/json" http-data=$payload http-max-redirect-count=0 duration=3s idle-timeout=2s output=none as-value]
                :if (($posted->"status") != "finished") do={ :error "guard GC acknowledgement incomplete" }
                :local elapsed ([:timestamp] - $opStarted)
                :local runs [/system scheduler get [find where name="szhome-mosdns-bgp-iso-writer"] run-count]
                :if (($elapsed > 300ms) || (($runs % 5) = 0)) do={ :log warning ("BGP-ISO-PROFILE op=" . $action . " next=" . $nextElapsed . " pre-ack=" . ($ackStarted - $opStarted) . " ack=" . ([:timestamp] - $ackStarted) . " total=" . $elapsed) }
            }
        } do={
            :local runs [/system scheduler get [find where name="szhome-mosdns-bgp-iso-writer"] run-count]
            :if (($runs % 5) = 0) do={ :log warning ("BGP-ISO-PROFILE writer-error=" . $writerErr) }
            # Unknown or stale evidence does not authorize a deletion. There
            # is no local cleanup; durable agent intent must be reconciled.
            :set writerContinue false
        }
        }
        # Keep route health in the same serialized job as mutations. A failed
        # mutation must not prevent independent health snapshots. The agent
        # validates snapshot identity, per-prefix generation and freshness.
        :onerror healthErr in={
            :local started [:timestamp]
            :local fetched [/tool fetch url="http://192.168.88.222:9080/guardwriter/health?bulk=1" src-address=192.168.88.254 http-max-redirect-count=0 duration=3s idle-timeout=2s output=user as-value]
            :if (($fetched->"status") != "finished") do={ :error "health fetch incomplete" }
            :local desired [:deserialize from=json options=json.no-string-conversion value=($fetched->"data")]
            :local epoch ($desired->"epoch")
            :local snapshotID ($desired->"snapshot_id")
            :local entries ($desired->"entries")
            :local healthPages ($desired->"total_pages")
            :if (([:typeof $epoch] != "str") || ([:typeof $snapshotID] != "str") || ([:typeof $entries] != "array") || ([:len $entries] > 512) || (($desired->"page") != 0) || ($healthPages != 1)) do={ :error "invalid health snapshot" }
            :if (([:len $epoch] != 24) || ([:len $snapshotID] != 24) || (($epoch ~ "^[0-9a-f]{24}") = false) || (($snapshotID ~ "^[0-9a-f]{24}") = false)) do={ :error "invalid health identity" }

            # Exactly one bulk snapshot per object class; no per-prefix ROS
            # find/get calls. An active competing route, duplicate guard or
            # malformed attributes keeps that prefix out of accepted.
            :local snapshotStarted [:timestamp]
            :local routes [/ip route print detail as-value where routing-table="rt-bgp-test" and active]
            :local guards [/ip route print detail as-value where comment~"^BGP-ISO-BH:"]
            :local activeCount [:toarray ""]
            :local activeOK [:toarray ""]
            :foreach route in=$routes do={
                :local prefix [:tostr ($route->"dst-address")]
                :if ([:typeof [:find $prefix "/"]] = "nil") do={ :set prefix ($prefix . "/32") }
                :if ([:typeof ($activeCount->$prefix)] = "nothing") do={ :set ($activeCount->$prefix) 0 }
                :set ($activeCount->$prefix) (($activeCount->$prefix) + 1)
                :if ((($route->"bgp") = true) && ([:tostr ($route->"gateway")] = "ovpn-szof") && ([:tostr ($route->"immediate-gw")] = "ovpn-szof") && (($route->"distance") = 20)) do={ :set ($activeOK->$prefix) true }
            }
            :local guardCount [:toarray ""]
            :local guardOK [:toarray ""]
            :local guardLabels [:toarray ""]
            :foreach route in=$guards do={
                :local prefix [:tostr ($route->"dst-address")]
                :if ([:typeof [:find $prefix "/"]] = "nil") do={ :set prefix ($prefix . "/32") }
                :if ([:typeof ($guardCount->$prefix)] = "nothing") do={ :set ($guardCount->$prefix) 0 }
                :set ($guardCount->$prefix) (($guardCount->$prefix) + 1)
                :local label [:tostr ($route->"comment")]
                :if ([:typeof ($guardLabels->$label)] = "nothing") do={ :set ($guardLabels->$label) 0 }
                :set ($guardLabels->$label) (($guardLabels->$label) + 1)
                :if ((($route->"static") = true) && (($route->"blackhole") = true) && (($route->"disabled") != true) && ([:tostr ($route->"routing-table")] = "rt-bgp-test") && (($route->"distance") = 240)) do={ :set ($guardOK->$label) $prefix }
            }
            :local accepted [:toarray ""]
            :local entryIndex 0
            :foreach entry in=$entries do={
                :local prefix [:tostr ($entry->"prefix")]
                :local gen ($entry->"generation")
                :if (([:typeof $gen] != "str") || ([:len $gen] != 24) || (($gen ~ "^[0-9a-f]{24}") = false)) do={ :error "invalid guard generation" }
                :local label ("BGP-ISO-BH:" . $gen . ":" . $prefix)
                :if ((($activeCount->$prefix) = 1) && (($activeOK->$prefix) = true) && (($guardCount->$prefix) = 1) && (($guardLabels->$label) = 1) && (($guardOK->$label) = $prefix)) do={ :set accepted ($accepted, $entryIndex) }
                :set entryIndex ($entryIndex + 1)
            }
            :if (([:timestamp] - $snapshotStarted) > 1s) do={ :error "health route snapshot too old" }
            :if (([:timestamp] - $started) > 2s) do={ :error "health request too old" }
            :local payload [:serialize to=json options=json.no-string-conversion value={"epoch"=$epoch;"snapshot_id"=$snapshotID;"accepted_indexes"=$accepted}]
            :if ([:len $payload] > 3900) do={ :error "health acknowledgement too large" }
            :local posted [/tool fetch url="http://192.168.88.222:9080/guardwriter/health-ack" src-address=192.168.88.254 http-method=post http-header-field="Content-Type:application/json" http-data=$payload http-max-redirect-count=0 duration=3s idle-timeout=2s output=none as-value]
            :if (($posted->"status") != "finished") do={ :error "health acknowledgement incomplete" }
            :local runs [/system scheduler get [find where name="szhome-mosdns-bgp-iso-writer"] run-count]
            :if ((([:timestamp] - $started) > 300ms) || (($runs % 5) = 0)) do={ :log warning ("BGP-ISO-PROFILE health total=" . ([:timestamp] - $started) . " accepted=" . [:len $accepted]) }
        } do={
            :local runs [/system scheduler get [find where name="szhome-mosdns-bgp-iso-writer"] run-count]
            :if (($runs % 5) = 0) do={ :log warning ("BGP-ISO-PROFILE health-error=" . $healthErr) }
            # Missing or delayed health must expire at the agent.
        }
    }
}

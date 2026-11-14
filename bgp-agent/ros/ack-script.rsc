# RouterOS 7.24.6. Install this named script before enabling its scheduler.
# No RouterOS credentials are sent to the BGP agent.
# The agent accepts ACK only from 192.168.88.254 and for its current epoch.
# /ip route active is a control-plane observation, not a packet-delivery proof.

/system script add name=bgp-test-ack policy=read,test comment="BGP-TEST active-route ACK" source={
    # A named scheduler invocation exposes this script name in /system script job.
    # A later invocation exits while an earlier GET/check/POST is still running.
    :if ([/system script job print count-only where script=[:jobname]] > 1) do={ :return }

    :do {
        :local fetched [/tool fetch url="http://192.168.88.223:9080/desired" src-address=192.168.88.254 http-max-redirect-count=0 duration=1s idle-timeout=1s output=user as-value]
        :if (($fetched->"status") != "finished") do={ :error "desired fetch incomplete" }
        :local desired [:deserialize from=json value=($fetched->"data")]
        :local epoch ($desired->"epoch")
        :local prefixes ($desired->"prefixes")
        :if (([:typeof $epoch] != "str") || ([:typeof $prefixes] != "array") || ([:len $prefixes] > 512)) do={ :error "invalid desired snapshot" }

        :local accepted [:toarray ""]
        :foreach prefix in=$prefixes do={
            :local found false
            :foreach route in=[/ip route find where dst-address=$prefix and routing-table="rt-bgp-test" and active and bgp] do={
                :local gateway [/ip route get $route gateway]
                :local immediate [/ip route get $route immediate-gw]
                :if (($gateway = "ovpn-szof") && ($immediate = "ovpn-szof")) do={ :set found true }
            }
            :if ($found) do={ :set accepted ($accepted, $prefix) }
        }

        :local payload [:serialize to=json options=json.no-string-conversion value={"epoch"=$epoch;"prefixes"=$accepted}]
        /tool fetch url="http://192.168.88.223:9080/ack" src-address=192.168.88.254 http-method=post http-header-field="Content-Type:application/json" http-data=$payload http-max-redirect-count=0 duration=1s idle-timeout=1s output=none
    } on-error={
        # Do not retain fresh ACKs locally. Agent ACK timestamps expire in 3 seconds.
        # Rate-limited logging is handled by watching scheduler run-count and state;
        # do not flood the router log every second during a deliberate peer outage.
    }
}

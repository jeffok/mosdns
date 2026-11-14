# RouterOS isolated BGP validation

These files target the observed szhome RouterOS 7.24.6 configuration. They are
operator-run artifacts, not a command to execute automatically during image build.
The main agent remains the only router writer.

`setup.rsc` creates `rt-bgp-test`, an explicit BGP instance and connection, filters,
bootstrap routes, source-limited connection/routing marks, and a disabled ACK
scheduler. It does not provision a container or change household DNS. The only
test source is `192.168.88.223`; existing LAN clients continue to use their current
routing rules. The agent receives no RouterOS management password.

`ack-script.rsc` installs the named script `bgp-test-ack`. Its GET `/desired` and
POST `/ack` use source `192.168.88.254` and bounded three-second fetch operations and two-second idle timeouts.
The script takes one `print detail as-value` snapshot of active BGP routes in the
dedicated table, builds an in-memory prefix set, and requires both `gateway` and
`immediate-gw` to equal `ovpn-szof`. Host destinations without a suffix are
normalized to `/32`. Verify these fields and serialization on the target ROS
version before enabling the scheduler. The ACK
contains a complete set, the agent's current epoch, and the 24-character
single-use `snapshot_id` from the same `/desired` response. The agent rejects
missing, reused, stale, or mismatched snapshots; freshness is measured from
issuance, not from delayed ACK arrival. It cannot acknowledge the
default route, old REST address-list state, or a matching static bootstrap route.

The named-job guard prevents overlapping scheduler instances and stale snapshot
overtaking. If a GET, lookup, or POST fails, the script does not refresh ACKs. The
agent expires old ACKs after its configured freshness window (the revised example uses six seconds measured from snapshot issuance). Slow successful cycles over 500 ms are sampled by scheduler run-count modulo 60,
with duration, snapshot size, desired count, and accepted count. An overlapping
run may cause a sample to be skipped; absence of a slow log does not prove
successful ACKs. Inspect agent state as well. A route snapshot taking longer
than one second, or desired snapshot older than two seconds before posting, is
rejected without refreshing ACKs.

Suggested operator order:

1. Record the existing DNS, route, mangle and container state without printing
   container environment variables. Build and archive the fixed image.
2. Set household DNS to `119.29.29.29` and cap ROS DNS cache at five minutes under
   the user's authorization; verify a domestic domain. This is a separate step
   and does not make overseas sites reachable by itself.
3. Provision agent `.223`, isolated persistent storage and configuration; import
   `setup.rsc`, then `ack-script.rsc`.
4. Confirm BGP Established, `rt-bgp-test` route next hops and filtering. Enable
   only `bgp-test-ack`, inspect scheduler `run-count`, script jobs and agent state.
5. Query the agent directly, verify returned TTLs and HTTP access from the agent
   source, then inspect that connections use `cm-bgp-test`, not `cm-ovpn` or
   FastTrack. Existing household clients are not proof of this isolated path.
6. Disable the ACK scheduler deliberately: new/expired ACKs must cause DNS
   SERVFAIL instead of an unconfirmed positive response. Restore it and verify
   recovery without changing a ROS password.
7. Exercise peer disconnect, restart and state replay. GC remains dry-run for
   normal domains; use explicitly isolated synthetic cases for withdrawal tests.

Route `active` is a control-plane observation, not proof that a packet was
forwarded. Verify first SYN, connection mark, SNAT interface and successful HTTPS
separately. A route can disappear between snapshot and packet transmission, so
the configured freshness window must not be advertised as an atomic FIB barrier.

Initial limits: 512 desired prefixes (<64 KiB fetch response), ten-second BGP
keepalive, thirty-second hold time, no automatic Graceful Restart assumption.
The GoBGP next hop can remain `.223` in its advertisement because ROS input
overrides it to the existing point-to-point OVPN interface. The router accepts no
default, aggregate, private, special-use or multicast route from this peer and
exports no routes back. China filtering happens in the agent; validation does not
install a full China route dataset into the ROS filter.

`rollback.rsc` removes only objects owned by this validation namespace. Container
and veth cleanup are separate after the container is stopped. It does not restore
household DNS: the operator must choose whether to retain temporary domestic DNS
or return to a previously validated resolver.

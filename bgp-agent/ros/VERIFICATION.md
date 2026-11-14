# Deployment isolation review

Read-only review of szhome RouterOS **7.24.6**, 2026-11-14 **09:12 Asia/Shanghai**.
The reviewer did not modify the router. The main operator was concurrently
exercising BGP peer and container restart behavior.

Observed isolation:

- Household DNS is only `119.29.29.29`, with `cache-max-ttl=5m`.
- Existing routing tables `main` and `rt-ovpn` remain; only `rt-bgp-test` was added.
- `main` retains its active `0.0.0.0/0` route through `pppoe-cn`, distance 1.
- The only configured BGP instance and neighbor are the validation `bgp-test`,
  local AS 64512, peer `192.168.88.223/32` AS 64513, using `rt-bgp-test`.
- No BGP route was installed in `main` in the review snapshot.
- The two test mangle rules match **only** source `192.168.88.223/32` on `br-lan`,
  exclude local destinations, and precede the existing `OVPN-proxy` rules.
- Test connections receive `cm-bgp-test` and use `rt-bgp-test`. The unchanged
  FastTrack rule requires `connection-mark=no-mark`, excluding these connections.
- Existing `OVPN-proxy` rules and `rt-ovpn` default route remain present.
- Existing `mosdns` and validation `mosdns-bgp-test` were both running at 09:12:10.
  The old resolver was using about 31.3 MiB and the test container about 12.6 MiB.
- ACK scheduler is enabled, interval 1 second, invoking named `bgp-test-ack` with
  policy `read,test`. Scheduler and script run-counts were increasing.
- Deployed script includes the named-job overlap guard, bounded GET/POST,
  complete prefix snapshot, active BGP route check, and exact OVPN gateway checks.
- Script job snapshots showed zero or one ACK job, not accumulating instances.

The main operator deliberately stopped the test container during this review.
Subsequent HTTP unavailability and BGP route withdrawal were expected restart-test
states and were not treated as a new deployment defect. End-to-end DNS, first SYN,
HTTPS, ACK failure closure and restart recovery results are owned by the main
operator's integration verification; this review establishes isolation only.

No container environment values, passwords, private keys or certificate contents
were read or recorded by this review.

# Production BGP preparation (NOT production-ready)

These files are review drafts. They were not imported or tested on RouterOS.
`prod-prestage.rsc` reserves a named, disabled BGP peer and filters; it does
not configure household DNS, create a scheduler or enable route exchange.
`prod-quiesce.rsc` disables only the owned writer scheduler and peer while
retaining guards. `prod-remove-prestage.rsc` removes unused pre-stage objects
only; it refuses to remove guard routes or barriers. A partial failed import
requires inspection before attempting cleanup. None of these files is a safe
rollback for an operational deployment with active client leases.

Names: instance and connection `szhome-mosdns-bgp`, filters
`szhome-mosdns-bgp-in` and `szhome-mosdns-bgp-out`, future script/scheduler
`szhome-mosdns-bgp-writer`; future guard namespace `BGP-PROD-BH:`. The existing
`rt-ovpn` and `cm-ovpn` are not modified.

## Mandatory gates before enabling

- The normal route ACK and lifecycle writer must be serialized by one proven
  implementation; the present guard coordinator and `/guardwriter/*` are not
  wired. No scheduler may run until restart reconciliation and end-to-end
  confirmation have passed. The application must pin a lease through actual DNS
  response completion; current code has no such callback.
- Verify that 183.56.180.152/32 (OVPN server) uses the telecom path even after
  peer establishment and tunnel loss. The filters reject this host and both
  8.8.8.8/32 and 1.1.1.1/32. Verify all other current VPN and DNS bootstrap
  endpoints as well, including changes to DNS names or resolved addresses.
- Verify existing /32 routes for 8.8.8.8 and 1.1.1.1 go through OVPN, and add
  separately owned, persistent higher-distance blackhole /32 backups only after
  checking duplicates, distance and route selection. Disable OVPN and verify
  those addresses select blackhole while 183.56.180.152 still uses telecom;
  restore OVPN and verify both resolvers use it again. No such backups are
  created by the pre-stage draft.
- Validate guard persistence and fail-closed behavior after BGP, tunnel,
  container and ROS restarts, including live conntrack, FastTrack and hardware
  offload. Dynamic route cleanup must not delete guards until DNS cache lifetime,
  generation ownership and connection draining are proven. BGP-learned /32s
  may affect existing cached addresses even before a new DNS answer is sent.
- Restrict peer/control traffic to the expected interfaces and source; source IP
  alone is not authentication and bridged traffic might bypass IP firewall.
  Confirm actual enforcement rather than assuming filter coverage.
- Test RouterOS 7.24.6 script parsing and commands on a disposable equivalent
  device. Import pre-stage, read all objects back, confirm the peer is disabled,
  no routes or scheduler are added, then rehearse quiesce/removal. Verify any
  partial failure leaves no unsafe or foreign changes. Rehearse on real router
  only under the operator's controlled rollback procedure.
- With the final writer in place, test parallel responses, at least 200 varied
  domain names, response transport/TTL, first-packet interface and SNAT, restart,
  ACK loss, overload, safe expiry and rollback. Confirm household domestic DNS
  policy separately. Preserve evidence and switch .222 into service only after
  these tests succeed.

The 512 input limit is a chosen deployment safety guard, not an inherent
RouterOS maximum. Evaluate capacity and exhaustion behavior before enabling.

## Single-writer integration draft

`prod-writer-draft.rsc` is an untested implementation draft of the coordinated
`/guardwriter/next` and `/guardwriter/ack` contract. It creates one script and
no scheduler. Both the script and mutations are source-gated off. HTTP 204 is
intended to mean idle; RouterOS fetch behavior for 204 must be verified on a
sandbox before use. An error yields no ACK, so the agent must retain and retry
the same immutable operation without concurrent work. An ACK carries the exact
operation identity and `proof`; `observe_withdrawn` also carries the parent ID.
The draft creates and checks a generation-owned guard and confirms the active
OVPN BGP route before acknowledging `create_guard`. The BGP advertisement must
already exist before that check, and this interval requires separate protection
for addresses clients already know. A script crash or lost ACK after deletion
can leave recovery blocked, which is deliberate pending durable receipts.

Run `python3 -m unittest bgp-agent/ros/test_prod_writer_draft.py` from the
repository root for static checks only. These do not parse or execute RouterOS
code, emulate concurrent operations, prove endpoint interoperability or prove
that a crash is safe. All gates above remain mandatory.

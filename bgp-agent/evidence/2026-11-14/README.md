# szhome BGP validation handoff — 2026-11-14

Status: experimental only; do not promote this implementation to household DNS or production routing. Source is branch `feat/bgp-validation`. The validation container was `jeffok/mosdns-bgp:bgp-validation-20261114.1`; its traffic alone used `192.168.88.223` and `rt-bgp-test`. Household ROS DNS remains `119.29.29.29`, cache-max-ttl 5m.

After the test, `mosdns-bgp-test` was stopped and `bgp-test-ack` scheduler disabled to avoid sustained route-scan load; test BGP routes were observed withdrawn (count 0). Image, configuration and route state persist. Re-enabling without inspecting state may restore hundreds of test routes. Do not modify household DNS, main table, or production routing during investigation.

## 200-domain test

`domains-tested.txt` lists the 200 unique names. `domains-results.jsonl` contains one record per domain. The probe, retained as `batch-probe.go.txt`, used concurrent UDP A queries (max 3) to the local validation DNS, then requested HTTPS on the first IPv4, with TLS verification and no redirect following. Status is an HTTP response, not proof that a whole site works. 68 DNS NOERROR, 130 SERVFAIL, and 2 client UDP unpack errors (www.douyin.com and www.sina.com.cn). For the 68 DNS NOERROR, HTTP: 18 200, 20 301, 8 302, 1 308, 8 403, 5 404, 1 501, 7 no response. TTL field starts at 300 and is minimized over answer records; it does not prove correctness of all TTL handling.

`domains-retest-tcp.jsonl` covers the two UDP-error domains and five SERVFAIL samples: both UDP-error domains and four SERVFAIL samples resolved via TCP; lsrelayaccess.com still SERVFAIL. The initial probe does not implement UDP TC-to-TCP fallback. `batch-probe-tcp.go.txt` is the exact TCP variant. Neither probe validates every address nor production domestic routing. No raw request secrets are included.

## Investigation findings and next steps

- At around 433 active BGP routes, the peer remained established while ROS CPU was observed at 67%; ACK script runs overlapped the 1-second scheduler. Its per-prefix route lookup and property reads are a likely scaling bottleneck. DNS confirmation budget is 2.5 s; many failures were near that limit and log `ROS route confirmation: context deadline exceeded`. This is a strong hypothesis, not a controlled causal proof. Profile and fix: batch-read active BGP routes, build an in-memory set and compare desired prefixes, preserve correct gateway and active-route checks. Do not rely solely on increasing timeout.
- Actual ROS logs reported `(scheduler:bgp-test-ack) Script Error: missing value(s) of argument(s) value (:return; line 4)` during overlap; inspect and correct the guard for RouterOS 7.24.6, and test overlap explicitly.
- UDP responses appear not to be truncated to the client's advertised UDP size; inspect `bgp-agent/main.go` response handling and reproduce with UDP clients before fixing. Honor EDNS size / default 512 and TC, retain full TCP answer, and update the probe to retry TCP on TC. Distinguish malformed/truncated responses from ACK failures.
- After fixing, rerun at least 200 domains and capture DNS code, UDP/TCP, response TTL, confirmation latency, CPU, route count, first-packet egress, route withdrawal/expiry, and existing route hit behavior. Validate positive, negative, concurrent, restart, lost ACK, route capacity and rollback behavior. Household DNS should remain independent during isolation tests. The global-only validation resolver does not validate domestic production split routing.

See `bgp-agent/ros/README.md` and `VERIFICATION.md` for deployed topology and prior observations. Treat any older assertion that the test container/scheduler is running as a historical snapshot.

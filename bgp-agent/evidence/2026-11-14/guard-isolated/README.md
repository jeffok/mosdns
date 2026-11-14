# Guard isolated test: initial failed run

RouterOS 7.24.6, single .222 container, separate rt-bgp-test; household DNS 119.29.29.29 and main table without BGP. 200 domains requested, concurrency 3; stopped early because most first results were SERVFAIL. See aborted-first.jsonl for actual completed responses, not a 200-domain result.

First github.com query timed out at 4.56 s; a later query returned an IPv4 and HTTPS 200. BGP routes and guard blackholes were observed in test table, .222 test mangle counters increased. ROS writer profiling showed ~1.0 s per /tool fetch, 2.08 s per create operation, and ~2.07 s for health. Packet capture showed HTTP response completed in ~2 ms while RouterOS fetch delayed closing for roughly 1 s; no SYN retransmission observed in the captured slow request. Content-Length, Connection: close, HTTP 200/204, and output=user/none did not reliably remove delays. Five data-directory create/write/fsync/rename/directory-fsync samples took 6.5–10.8 ms total.

Journal inspection after cancelled queries: PREPARING 23, GUARDED 15, ACTIVE 2. This exposed orphan cleanup requirements. Production cutover has not been performed.

Second run used batch writer and a 12-second agent confirmation budget, 14-second plugin budget and 25-second test client timeout. It was stopped early after repeated SERVFAIL; nearly all failed at 5001–5004 ms. The upstream mosdns entry handler independently imposes a 5-second deadline on the entire query; this cancels the route confirmation before those longer budgets. This is a separate cause from ROS fetch latency. The run did not complete 200 domains.

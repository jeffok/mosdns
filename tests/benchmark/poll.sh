#!/bin/sh
ms() { awk '{printf "%.0f\n", $1 * 1000}' /proc/uptime; }
while [ ! -f /tmp/bench-poll-stop ]; do
  a=$(ms)
  ans=$(dig @127.0.0.1 bench.p2.invalid +time=1 +tries=1 +short 2>/dev/null)
  b=$(ms)
  if [ "$ans" = 192.0.2.77 ]; then ok=1; else ok=0; fi
  printf '%s\t%s\t%s\n' "$a" "$b" "$ok" >> "$1"
  sleep 0.02
done

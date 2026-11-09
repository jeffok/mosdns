#!/bin/sh
# Run only under BusyBox in the arm64 tools image, one fresh container/trial.
set -eu
OUT=${BENCH_OUT:-/evidence}
mkdir -p "$OUT" /etc/mosdns/rules /data/state
cp /opt/mosdns-rules/* /etc/mosdns/rules/
printf 'bench.p2.invalid 192.0.2.77\n' > /etc/mosdns/rules/custom-hosts.txt
export CUSTOM_HOSTS_SOURCES=http://127.0.0.1:9/disabled
export CUSTOM_HOSTS_INTERVAL=86400 RULES_UPDATE_INTERVAL=86400
export RULES_SOURCES=http://127.0.0.1:9/disabled
export CACHE_DUMP_FILE=/data/cache.dump
export CANARY_SENTINEL=bench.p2.invalid MOSDNS_LOG_FILE='""'
ms() { awk '{printf "%.0f\n", $1 * 1000}' /proc/uptime; }
healthy() { dig @127.0.0.1 bench.p2.invalid +tries=1 +time=1 +short | grep -qx 192.0.2.77; }
ready() {
  end=$(( $(ms) + 30000 ))
  until healthy; do [ "$(ms)" -lt "$end" ] || return 1; sleep 0.02; done
}
t0=$(ms)
sh /opt/mosdns/entrypoint.sh > "$OUT/entrypoint.log" 2>&1 &
ep=$!
trap 'kill -TERM "$ep" 2>/dev/null || true; wait "$ep" 2>/dev/null || true' EXIT INT TERM
ready
t1=$(ms)
echo "cold_ms=$((t1-t0))" | tee "$OUT/results.txt"
wc -l /etc/mosdns/rules/* > "$OUT/rule-counts.txt"
# A fixed real-domain workload; external replies are observations only.
for d in www.baidu.com www.qq.com www.google.com apps.apple.com; do
  dig @127.0.0.1 "$d" +time=1 +tries=1 > "$OUT/sentinel-$d.txt" || true
done
grep -v '^#' /etc/mosdns/rules/cn-domains.txt | head -200 > /tmp/workload.txt
xargs -P 8 -n 1 sh /bench/query.sh < /tmp/workload.txt
pid=$(cat /tmp/mosdns.pid)
awk '$1=="anon" {printf "container_anon_kib=%.0f\n", $2/1024}' /sys/fs/cgroup/memory.stat | tee -a "$OUT/results.txt"
awk '/VmRSS:/ { print "rss_kib=" $2 }' "/proc/$pid/status" | tee -a "$OUT/results.txt"
curl -fsS http://127.0.0.1:9091/plugins/lazy_cache/dump > "$OUT/cache-api.dump"
stat -c 'api_dump_bytes=%s' "$OUT/cache-api.dump" | tee -a "$OUT/results.txt"
t0=$(ms)
kill -TERM "$pid"
ready
# Ensure a genuinely new process answered, rather than an old socket.
while [ "$(cat /tmp/mosdns.pid)" = "$pid" ]; do sleep 0.02; done
ready
t1=$(ms)
echo "restart_ms=$((t1-t0))" | tee -a "$OUT/results.txt"
stat -c 'dump_bytes=%s' /data/cache.dump | tee -a "$OUT/results.txt"
curl -fsS http://127.0.0.1:9091/metrics > "$OUT/metrics-after-restart.txt"
cp /data/cache.dump "$OUT/cache-sigterm.dump"
cp -a /etc/mosdns/rules /data/state/staging
printf '\n# benchmark change\n' >> /data/state/staging/custom-local.txt
# Loop dig throughout apply. Each row is elapsed start, elapsed finish, success.
: > "$OUT/poll.tsv"
sh /bench/poll.sh "$OUT/poll.tsv" &
poll=$!
sleep 0.2
set +e
sh /opt/mosdns/apply.sh --now > "$OUT/apply.log" 2>&1
rc=$?
set -e
sleep 0.2
touch /tmp/bench-poll-stop
wait "$poll"
echo "apply_rc=$rc" | tee -a "$OUT/results.txt"
awk '$3==0 { if(!n) first=$1; last=$2; n++; } END { printf "failed_queries=%d\nfailure_window_ms=%d\n", n, n ? last-first : 0 }' "$OUT/poll.tsv" | tee -a "$OUT/results.txt"
awk 'NR>1 && prev==1 && $3==0 { start=pt } NR>1 && prev==0 && $3==1 { gap=$2-start; if(gap>max)max=gap } {prev=$3;pt=$1} END {print "outage_upper_bound_ms=" max+0}' "$OUT/poll.tsv" | tee -a "$OUT/results.txt"
exit "$rc"

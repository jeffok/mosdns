#!/bin/sh
set -eu
ROOT=${MOSDNS_ROOT:?}
for trial in 1 2 3; do
  echo "TRIAL=$trial"
  docker run --rm --name "mosdns-p2-benchmark-$trial" --platform linux/arm64 \
    --entrypoint /bin/sh -e "BENCH_OUT=/evidence/trial$trial" \
    -v "$ROOT/scripts:/opt/mosdns:ro" \
    -v "$ROOT/tests/benchmark:/bench:ro" \
    -v "$ROOT/.cursor/tmp/p2-benchmark:/evidence" \
    mosdns:p2-tools /bench/benchmark.sh
done

#!/bin/sh
set -eu
MOSDNS_IMAGE=${MOSDNS_IMAGE:-mosdns:p2-tools}
export MOSDNS_IMAGE
exec /bin/sh "${MOSDNS_REPO:-$PWD}/tests/run.sh"

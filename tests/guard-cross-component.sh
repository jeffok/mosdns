#!/bin/sh
# Uses isolated local processes; never touches the router or running containers.
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
W=$(mktemp -d)
trap 'rm -rf "$W"' EXIT INT TERM
[ -n "${MOSDNS_GUARD_E2E_BIN:-}" ] || { echo 'set MOSDNS_GUARD_E2E_BIN to the patched native mosdns binary' >&2; exit 1; }
cp -R "$ROOT/bgp-agent" "$W/agent"
cp "$ROOT/tests/guard-cross-component.test.go" "$W/agent/guard_cross_component_test.go"
cd "$W/agent"
go test -race -run '^TestCrossComponentGuardDNS$' -v .

#!/bin/sh
# Build the fixed BGP validation image and an importable RouterOS archive.
# No network configuration or router deployment is performed here.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
VERSION=bgp-validation-20261114.1
TAG="jeffok/mosdns-bgp:$VERSION"
PLATFORM=${PLATFORM:-linux/arm64}
OUT_DIR=${OUT_DIR:-$ROOT/.cache/bgp-agent/$VERSION}

case "$PLATFORM" in
  linux/arm64) ARCH=arm64 ;;
  linux/amd64) ARCH=amd64 ;;
  *) echo "Supported PLATFORM: linux/arm64 or linux/amd64" >&2; exit 2 ;;
esac
command -v docker >/dev/null
command -v python3 >/dev/null
mkdir -p "$OUT_DIR"

docker buildx build \
  --platform "$PLATFORM" --load --provenance=false --sbom=false \
  --tag "$TAG" --file "$ROOT/bgp-agent/Dockerfile" "$ROOT/bgp-agent"

docker image inspect "$TAG" --format '{{.Id}} {{.Os}}/{{.Architecture}}' \
  > "$OUT_DIR/image-$ARCH.txt"
docker save "$TAG" --output "$OUT_DIR/image-$ARCH.oci.tar"
python3 "$ROOT/tools/ros-docker-archive.py" \
  "$OUT_DIR/image-$ARCH.oci.tar" "$OUT_DIR/bgp-agent-$ARCH.tar" "$TAG"
cp "$ROOT/bgp-agent/config.example.json" "$OUT_DIR/config.example.json"

# Capture source identity in addition to the fixed version; no Git mutation.
git -C "$ROOT" rev-parse HEAD > "$OUT_DIR/source-head.txt"
git -C "$ROOT" diff -- bgp-agent tools/build-bgp-agent.sh > "$OUT_DIR/source-tracked.diff"
python3 - "$ROOT" "$OUT_DIR" "$ARCH" <<'PY'
import hashlib
import json
from pathlib import Path
import sys

root, out, arch = Path(sys.argv[1]), Path(sys.argv[2]), sys.argv[3]
sources = sorted((root / "bgp-agent").glob("*.go"))
sources += [root / "bgp-agent" / n for n in ("go.mod", "go.sum", "Dockerfile", "config.example.json")]
sources += [root / "tools" / "build-bgp-agent.sh", root / "tools" / "ros-docker-archive.py"]
manifest = {str(p.relative_to(root)): hashlib.sha256(p.read_bytes()).hexdigest() for p in sources}
(out / "source-sha256.json").write_text(json.dumps(manifest, indent=2) + "\n")
names = [f"bgp-agent-{arch}.tar", f"image-{arch}.oci.tar", f"image-{arch}.txt", "config.example.json", "source-head.txt", "source-tracked.diff", "source-sha256.json"]
with (out / f"SHA256SUMS-{arch}").open("w") as f:
    for name in names:
        f.write(f"{hashlib.sha256((out / name).read_bytes()).hexdigest()}  {name}\n")
print(f"Artifacts: {out}")
PY

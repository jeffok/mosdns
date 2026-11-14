#!/bin/sh
# 把本仓库的 ros_bgp 插件拼进固定 commit 的上游 mosdns 并编译。
# 单元测试在构建机原生架构执行；只有 build 子命令设置目标 GOOS/GOARCH。
set -eu

REF=${MOSDNS_REF:-v5.3.4}
COMMIT=${MOSDNS_COMMIT:-b7323188bab1ea742538aeccb31b692bc4967d1b}
ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
PLUGIN_SRC=${PLUGIN_SRC:-$ROOT/plugins/ros_bgp}
UPSTREAM_DIR=${UPSTREAM_DIR:-$ROOT/.cache/mosdns-upstream}
CMD=${1:-prepare}

prepare() {
  if [ ! -d "$UPSTREAM_DIR/.git" ]; then
    rm -rf "$UPSTREAM_DIR"
    git clone --depth 1 --branch "$REF" https://github.com/IrineSistiana/mosdns.git "$UPSTREAM_DIR"
  fi
  got=$(git -C "$UPSTREAM_DIR" rev-parse HEAD)
  if [ "$got" != "$COMMIT" ]; then
    echo "上游 commit 不符: $got ，期望 $COMMIT" >&2
    exit 1
  fi
  patch="$ROOT/patches/mosdns-response-source.patch"
  [ -s "$patch" ] || { echo "missing response source patch" >&2; exit 1; }
  if git -C "$UPSTREAM_DIR" apply --check "$patch" 2>/dev/null; then
    git -C "$UPSTREAM_DIR" apply "$patch"
  elif ! git -C "$UPSTREAM_DIR" apply --reverse --check "$patch" 2>/dev/null; then
    echo "response source patch does not match pinned upstream" >&2; exit 1
  fi
  cp "$ROOT/patches/fallback_route_source_test.go" "$UPSTREAM_DIR/plugin/executable/sequence/fallback/route_source_test.go"
  rm -rf "$UPSTREAM_DIR/plugin/executable/ros_bgp"
  mkdir -p "$UPSTREAM_DIR/plugin/executable/ros_bgp"
  cp "$PLUGIN_SRC"/*.go "$UPSTREAM_DIR/plugin/executable/ros_bgp/"
  f="$UPSTREAM_DIR/plugin/enabled_plugins.go"
  git -C "$UPSTREAM_DIR" show "$COMMIT:plugin/enabled_plugins.go" > "$f.tmp"
  mv "$f.tmp" "$f"
  if ! grep -q 'executable/ros_bgp' "$f"; then
    grep -q 'executable/reverse_lookup' "$f" || {
      echo "enabled_plugins.go 找不到 reverse_lookup 锚点" >&2
      exit 1
    }
    awk '
      { print }
      index($0, "executable/reverse_lookup") && !done {
        print "\t_ \"github.com/IrineSistiana/mosdns/v5/plugin/executable/ros_bgp\""
        done = 1
      }
    ' "$f" > "$f.tmp"
    mv "$f.tmp" "$f"
  fi
  grep -q 'executable/ros_bgp' "$f"
  echo "prepared $UPSTREAM_DIR @ $got"
}

build() {
  [ -d "$UPSTREAM_DIR/plugin/executable/ros_bgp" ] || prepare
  out=${OUT:-/out/mosdns}
  mkdir -p "$(dirname "$out")"
  (
    cd "$UPSTREAM_DIR"
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${REF}+ros_bgp" -o "$out" .
  )
  echo "built $out"
}

case "$CMD" in
  prepare) prepare ;;
  build) build ;;
  *) echo "用法: $0 prepare|build" >&2; exit 1 ;;
esac

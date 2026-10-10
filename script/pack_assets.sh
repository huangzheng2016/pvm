#!/usr/bin/env bash
# pack_assets.sh builds the native host's linux guest agent and packs it
# (zstd + manifest with sha256) into internal/assets/packed for go:embed.
# The canonical full build entrypoint is script/build.sh.
set -euo pipefail
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
. "$SCRIPT_DIR/lib/native.sh"
pvm_native_target
cd "$SCRIPT_DIR/.."

OUT=internal/assets/packed
mkdir -p "$OUT"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

name="agent-linux-$PVM_HOSTARCH"
echo "packing $name"
CGO_ENABLED=0 GOOS=linux GOARCH="$PVM_HOSTARCH" go build -trimpath -ldflags "-s -w" -o "$TMP/$name" ./cmd/pvm
size=$(stat -f%z "$TMP/$name" 2>/dev/null || stat -c%s "$TMP/$name")
echo "  raw size: $((size/1024/1024)) MB"
payload="$OUT/$name.zst"
zstd -19 -q -f "$TMP/$name" -o "$payload"
if command -v shasum >/dev/null 2>&1; then
  sum=$(shasum -a 256 "$TMP/$name" | cut -d' ' -f1)
else
  sum=$(sha256sum "$TMP/$name" | cut -d' ' -f1)
fi
printf '[\n {"path": "%s", "sha256": "%s", "mode": 493}\n]\n' "$name" "$sum" > "$OUT/manifest.json"
echo "manifest:"
cat "$OUT/manifest.json"

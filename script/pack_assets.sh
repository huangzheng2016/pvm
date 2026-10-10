#!/usr/bin/env bash
# pack_assets.sh builds the native host's linux guest agent and packs it
# (zstd + manifest with sha256) into internal/assets/packed for go:embed.
# The canonical full build entrypoint is script/build.sh.
set -euo pipefail
cd "$(dirname "$0")/.."

OUT=internal/assets/packed
mkdir -p "$OUT"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

arch=$(go env GOARCH)
name="agent-linux-$arch"
echo "packing $name"
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -ldflags "-s -w" -o "$TMP/$name" ./cmd/pvm
size=$(stat -f%z "$TMP/$name")
echo "  raw size: $((size/1024/1024)) MB"
payload="$OUT/$name.zst"
zstd -19 -q -f "$TMP/$name" -o "$payload"
sum=$(shasum -a 256 "$TMP/$name" | cut -d' ' -f1)
printf '[\n {"path": "%s", "sha256": "%s", "mode": 493}\n]\n' "$name" "$sum" > "$OUT/manifest.json"
echo "manifest:"
cat "$OUT/manifest.json"

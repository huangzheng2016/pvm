#!/usr/bin/env bash
# pack_assets.sh cross-compiles the linux guest agent binaries and
# packs them (zstd + manifest with sha256) into internal/assets/packed
# for go:embed. Run before building the host binary; script/build.sh
# does both.
set -euo pipefail
cd "$(dirname "$0")/.."

OUT=internal/assets/packed
mkdir -p "$OUT"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

manifest="$TMP/manifest.json"
echo '[' > "$manifest"
first=1
for arch in amd64 arm64; do
  name="agent-linux-$arch"
  echo "packing $name"
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags "-s -w" -o "$TMP/$name" ./cmd/pvm
  size=$(stat -f%z "$TMP/$name")
  echo "  raw size: $((size/1024/1024)) MB"
  payload="$OUT/$name.zst"
  zstd -19 -q -f "$TMP/$name" -o "$payload"
  sum=$(shasum -a 256 "$TMP/$name" | cut -d' ' -f1)
  [ $first -eq 0 ] && echo ',' >> "$manifest"
  first=0
  printf ' {"path": "%s", "sha256": "%s", "mode": 493}\n' "$name" "$sum" >> "$manifest"
done
echo ']' >> "$manifest"
mv "$manifest" "$OUT/manifest.json"
echo "manifest:"
cat "$OUT/manifest.json"

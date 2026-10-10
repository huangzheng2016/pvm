#!/usr/bin/env bash
# pack_assets.sh builds the native linux guest agent binary and packs it
# (zstd + manifest with sha256) into internal/assets/packed
# for go:embed. Run before building the host binary; script/build.sh
# does both.
set -euo pipefail
cd "$(dirname "$0")/.."

OUT=internal/assets/packed
mkdir -p "$OUT"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

case "$(uname -m)" in
  arm64|aarch64) HOSTARCH=arm64 ;;
  x86_64|amd64) HOSTARCH=amd64 ;;
  *) echo "unsupported host architecture: $(uname -m)" >&2; exit 1 ;;
esac

manifest="$TMP/manifest.json"
name="agent-linux-$HOSTARCH"
echo "packing $name"
CGO_ENABLED=0 GOOS=linux GOARCH="$HOSTARCH" go build -trimpath -ldflags "-s -w" -o "$TMP/$name" ./cmd/pvm
size=$(stat -f%z "$TMP/$name")
echo "  raw size: $((size/1024/1024)) MB"
payload="$OUT/$name.zst"
zstd -19 -q -f "$TMP/$name" -o "$payload"
sum=$(shasum -a 256 "$TMP/$name" | cut -d' ' -f1)
printf '[\n {"path": "%s", "sha256": "%s", "mode": 493}\n]\n' "$name" "$sum" > "$manifest"
mv "$manifest" "$OUT/manifest.json"
echo "manifest:"
cat "$OUT/manifest.json"

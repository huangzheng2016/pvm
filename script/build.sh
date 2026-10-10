#!/usr/bin/env bash
# build.sh: cross-compile guest binaries (gateway + agent), pack assets,
# then build the host pvm binary with embedded assets.
set -euo pipefail
cd "$(dirname "$0")/.."

HOSTARCH=$(go env GOARCH)
if [ "${PVM_ALL_ARCH:-0}" = "1" ]; then
  ARCHES="amd64 arm64"
else
  ARCHES="$HOSTARCH"
fi

# Remove stale raw/compressed guests so a native build never embeds the other
# architecture left over from an earlier PVM_ALL_ARCH build.
rm -f internal/assets/packed/gateway-linux-* internal/assets/packed/agent-linux-*

echo "== guest binaries ($ARCHES)"
for arch in $ARCHES; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -ldflags='-s -w' -o "internal/assets/packed/gateway-linux-$arch" ./cmd/gateway
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -ldflags='-s -w' -o "internal/assets/packed/agent-linux-$arch" ./cmd/pvm
done

echo "== assetpack"
go run ./cmd/assetpack
# Only the compressed payloads and manifest are embedded; raw guests are
# build intermediates and would otherwise be embedded a second time. Keep
# the .zst payloads: the glob above matches them too, so filter by suffix.
find internal/assets/packed -maxdepth 1 -type f \( -name 'gateway-linux-*' -o -name 'agent-linux-*' \) ! -name '*.zst' -delete
for arch in $ARCHES; do
  for kind in gateway agent; do
    payload="internal/assets/packed/$kind-linux-$arch.zst"
    [ -s "$payload" ] || { echo "missing packed asset: $payload" >&2; exit 1; }
  done
done

echo "== pvm"
go build -tags embedassets -buildvcs=false -trimpath -ldflags "-s -w -buildid=" -o pvm ./cmd/pvm
echo "built ./pvm (with embedded guest assets: $ARCHES)"

if [ "${PVM_PACK_QEMU:-1}" = "1" ] && [ "$(uname -s)" = "Darwin" ] && command -v brew >/dev/null 2>&1; then
  echo "== bundled qemu ($HOSTARCH)"
  ./script/qemu/pack-runtime.sh
else
  echo "== bundled qemu: skipped (PVM_PACK_QEMU=0 or unsupported host)"
fi

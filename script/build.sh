#!/usr/bin/env bash
# build.sh: build native guest binaries (gateway + agent), pack assets,
# then build the host pvm binary with embedded assets.
set -euo pipefail
cd "$(dirname "$0")/.."

case "$(uname -m)" in
  arm64|aarch64) HOSTARCH=arm64 ;;
  x86_64|amd64) HOSTARCH=amd64 ;;
  *) echo "unsupported host architecture: $(uname -m)" >&2; exit 1 ;;
esac
case "$(uname -s)" in
  Darwin) HOSTOS=darwin ;;
  Linux) HOSTOS=linux ;;
  *) echo "unsupported host OS: $(uname -s)" >&2; exit 1 ;;
esac

# Remove stale raw/compressed guests so a native build never embeds
# another architecture left over from an earlier multi-architecture build.
rm -f internal/assets/packed/gateway-linux-* internal/assets/packed/agent-linux-*

echo "== guest binaries ($HOSTARCH)"
CGO_ENABLED=0 GOOS=linux GOARCH="$HOSTARCH" go build -trimpath -ldflags='-s -w' -o "internal/assets/packed/gateway-linux-$HOSTARCH" ./cmd/gateway
CGO_ENABLED=0 GOOS=linux GOARCH="$HOSTARCH" go build -trimpath -ldflags='-s -w' -o "internal/assets/packed/agent-linux-$HOSTARCH" ./cmd/pvm

echo "== assetpack"
go run ./cmd/assetpack
# Only the compressed payloads and manifest are embedded; raw guests are
# build intermediates and would otherwise be embedded a second time. Keep
# the .zst payloads: the glob above matches them too, so filter by suffix.
find internal/assets/packed -maxdepth 1 -type f \( -name 'gateway-linux-*' -o -name 'agent-linux-*' \) ! -name '*.zst' -delete
for kind in gateway agent; do
  payload="internal/assets/packed/$kind-linux-$HOSTARCH.zst"
  [ -s "$payload" ] || { echo "missing packed asset: $payload" >&2; exit 1; }
done

echo "== pvm"
GOOS="$HOSTOS" GOARCH="$HOSTARCH" go build -tags embedassets -buildvcs=false -trimpath -ldflags "-s -w -buildid=" -o pvm ./cmd/pvm
echo "built ./pvm (with embedded guest assets: $HOSTARCH)"

if [ "${PVM_PACK_QEMU:-1}" = "1" ] && [ "$(uname -s)" = "Darwin" ] && command -v brew >/dev/null 2>&1; then
  echo "== bundled qemu ($HOSTARCH)"
  ./script/qemu/pack-runtime.sh
else
  echo "== bundled qemu: skipped (PVM_PACK_QEMU=0 or unsupported host)"
fi

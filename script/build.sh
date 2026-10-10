#!/usr/bin/env bash
# build.sh: build the native host's guest binaries (gateway + agent),
# pack assets, then build the host pvm binary with embedded assets.
set -euo pipefail
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
. "$SCRIPT_DIR/lib/native.sh"
pvm_native_target
cd "$SCRIPT_DIR/.."

# Remove stale raw/compressed guests from previous builds.
rm -f internal/assets/packed/gateway-linux-* internal/assets/packed/agent-linux-*

echo "== guest binaries ($PVM_HOSTARCH)"
CGO_ENABLED=0 GOOS=linux GOARCH="$PVM_HOSTARCH" go build -trimpath -ldflags='-s -w' -o "internal/assets/packed/gateway-linux-$PVM_HOSTARCH" ./cmd/gateway
CGO_ENABLED=0 GOOS=linux GOARCH="$PVM_HOSTARCH" go build -trimpath -ldflags='-s -w' -o "internal/assets/packed/agent-linux-$PVM_HOSTARCH" ./cmd/pvm

echo "== assetpack"
go run ./cmd/assetpack
# Only the compressed payloads and manifest are embedded; raw guests are
# build intermediates and would otherwise be embedded a second time.
rm -f internal/assets/packed/gateway-linux-* internal/assets/packed/agent-linux-*

echo "== pvm"
GOOS="$PVM_HOSTOS" GOARCH="$PVM_HOSTARCH" go build -tags embedassets -buildvcs=false -trimpath -ldflags "-s -w -buildid=" -o pvm ./cmd/pvm
echo "built ./pvm (with embedded guest assets: $PVM_HOSTARCH)"

if [ "${PVM_PACK_QEMU:-1}" = "1" ] && [ "$(uname -s)" = "Darwin" ] && command -v brew >/dev/null 2>&1; then
  echo "== bundled qemu ($PVM_HOSTARCH)"
  ./script/qemu/pack-runtime.sh
else
  echo "== bundled qemu: skipped (PVM_PACK_QEMU=0 or unsupported host)"
fi

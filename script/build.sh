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
rm -f "internal/assets/packed/gateway-linux-$PVM_HOSTARCH" "internal/assets/packed/agent-linux-$PVM_HOSTARCH"
for kind in gateway agent; do
  payload="internal/assets/packed/$kind-linux-$PVM_HOSTARCH.zst"
  [ -s "$payload" ] || { echo "missing packed asset: $payload" >&2; exit 1; }
done

echo "== pvm"
GOOS="$PVM_HOSTOS" GOARCH="$PVM_HOSTARCH" go build -tags embedassets -buildvcs=false -trimpath -ldflags "-s -w -buildid=" -o pvm ./cmd/pvm
echo "built ./pvm (with embedded guest assets: $PVM_HOSTARCH)"

# Prebuilt linux/amd64 CLI for deploying pvm exit-agent to a VPS over ssh
# (see pvm-skill "给定 SSH 部署 exit"). This is a distribution artifact,
# not cross-arch VM support: it is never injected into guests.
echo "== dist deploy bundle (linux/amd64)"
mkdir -p dist "$HOME/.pvm/runtime/dist"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags "-s -w -buildid=" -o dist/pvm-linux-amd64 ./cmd/pvm
cp dist/pvm-linux-amd64 "$HOME/.pvm/runtime/dist/pvm-linux-amd64"

if [ "${PVM_PACK_QEMU:-1}" = "1" ] && [ "$(uname -s)" = "Darwin" ] && command -v brew >/dev/null 2>&1; then
  echo "== bundled qemu ($PVM_HOSTARCH)"
  ./script/qemu/pack-runtime.sh
else
  echo "== bundled qemu: skipped (PVM_PACK_QEMU=0 or unsupported host)"
fi

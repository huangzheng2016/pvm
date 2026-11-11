#!/usr/bin/env bash
# build.sh: build the native host's guest binaries (gateway + agent),
# pack assets, then build the host pvm binary with embedded assets.
set -euo pipefail
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
. "$SCRIPT_DIR/lib/native.sh"
pvm_native_target
cd "$SCRIPT_DIR/.."
PVM_VERSION="${PVM_VERSION:-$(cat VERSION)}"

# Remove stale raw/compressed guests from previous builds.
rm -f internal/assets/packed/gateway-linux-* internal/assets/packed/agent-linux-*

echo "== guest binaries ($PVM_HOSTARCH)"
PVM_GARBLE_VERSION=v0.18.0
PVM_GARBLE_SEED=cHZtZ3Vlc3Q=
if ! command -v garble >/dev/null 2>&1; then
  echo "error: garble $PVM_GARBLE_VERSION is required: go install mvdan.cc/garble@$PVM_GARBLE_VERSION" >&2
  exit 1
fi
garble_version=$(garble version)
garble_first_line=${garble_version%%$'\n'*}
if [ "$garble_first_line" != "mvdan.cc/garble $PVM_GARBLE_VERSION" ]; then
  echo "error: found $garble_first_line; install mvdan.cc/garble@$PVM_GARBLE_VERSION" >&2
  exit 1
fi
CGO_ENABLED=0 GOOS=linux GOARCH="$PVM_HOSTARCH" go build -trimpath -buildvcs=false -ldflags='-s -w -buildid=' -o "internal/assets/packed/gateway-linux-$PVM_HOSTARCH" ./cmd/gateway
CGO_ENABLED=0 GOOS=linux GOARCH="$PVM_HOSTARCH" garble -tiny -seed="$PVM_GARBLE_SEED" build -trimpath -buildvcs=false -ldflags='-s -w -buildid=' -o "internal/assets/packed/agent-linux-$PVM_HOSTARCH" ./cmd/agent
agent_bin="internal/assets/packed/agent-linux-$PVM_HOSTARCH"
if ! go version -m "$agent_bin" >/dev/null 2>&1; then
  echo "error: cannot read guest Go metadata" >&2
  exit 1
fi
if { strings "$agent_bin"; go version -m "$agent_bin"; } | grep -Ei 'github\.com/huangzheng2016|huangzheng' >/dev/null; then
  echo "error: obfuscated guest contains repository identifiers" >&2
  exit 1
fi

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
pvm_out=pvm
[ "$PVM_HOSTOS" = "windows" ] && pvm_out=pvm.exe
GOOS="$PVM_HOSTOS" GOARCH="$PVM_HOSTARCH" go build -tags embedassets -buildvcs=false -trimpath \
  -ldflags "-s -w -buildid= -X github.com/huangzheng2016/pvm/internal/buildinfo.Version=${PVM_VERSION:-dev}" \
  -o "$pvm_out" ./cmd/pvm
echo "built ./$pvm_out (with embedded guest assets: $PVM_HOSTARCH)"

if [ "${PVM_PACK_QEMU:-1}" = "1" ] && [ "$(uname -s)" = "Darwin" ] && command -v brew >/dev/null 2>&1; then
  echo "== bundled qemu ($PVM_HOSTARCH)"
  ./script/qemu/pack-runtime.sh
else
  echo "== bundled qemu: skipped (PVM_PACK_QEMU=0 or unsupported host)"
fi

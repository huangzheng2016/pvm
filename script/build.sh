#!/usr/bin/env bash
# build.sh: pack guest assets, then build the host pvm binary.
set -euo pipefail
cd "$(dirname "$0")/.."
./script/pack_assets.sh
go build -tags embedassets -trimpath -ldflags "-s -w" -o pvm ./cmd/pvm
echo "built ./pvm (with embedded guest assets)"

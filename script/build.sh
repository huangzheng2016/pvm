#!/usr/bin/env bash
# build.sh: cross-compile guest binaries (gateway + agent), pack assets,
# then build the host pvm binary with embedded assets.
set -euo pipefail
cd "$(dirname "$0")/.."

echo "== guest binaries (linux arm64+amd64)"
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o internal/assets/packed/gateway-linux-arm64 ./cmd/gateway
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o internal/assets/packed/gateway-linux-amd64 ./cmd/gateway
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o internal/assets/packed/agent-linux-arm64 ./cmd/pvm
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o internal/assets/packed/agent-linux-amd64 ./cmd/pvm

echo "== assetpack"
go run ./cmd/assetpack

echo "== pvm"
go build -tags embedassets -trimpath -ldflags "-s -w" -o pvm ./cmd/pvm
echo "built ./pvm (with embedded guest assets)"

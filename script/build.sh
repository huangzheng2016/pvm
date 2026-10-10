#!/usr/bin/env bash
# pvm build: cross-compile guest binaries, pack assets, build pvm.
set -euo pipefail
cd "$(dirname "$0")/.."

echo "== guest binaries (linux arm64+amd64)"
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o internal/assets/packed/gateway-linux-arm64 ./cmd/gateway
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o internal/assets/packed/gateway-linux-amd64 ./cmd/gateway

echo "== assetpack"
go run ./cmd/assetpack

echo "== pvm"
go build -o pvm ./cmd/pvm
echo "ok: ./pvm"

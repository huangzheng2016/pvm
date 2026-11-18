#!/usr/bin/env bash
# build-desktop.sh: build pvm-desktop.exe, the Windows desktop app. It is a
# cgo-free cross-build, so it runs on any host with Go and Node (npm):
# the frontend first, then the Go program that embeds it.
set -euo pipefail
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
cd "$SCRIPT_DIR/.."
PVM_VERSION=$(printf %s "${PVM_VERSION:-$(cat VERSION)}" | tr -d '\r\n')
PVM_RSRC_VERSION=v0.10.2
app=cmd/pvm-desktop

echo "== frontend"
(cd "$app/frontend" && npm ci --no-audit --no-fund && npm run build)
asset=$(find "$app/frontend/dist" -type f ! -name index.html | LC_ALL=C sort | head -n 1)
[ -n "$asset" ] || { echo "error: the frontend build left no assets in $app/frontend/dist" >&2; exit 1; }

echo "== pvm-desktop.exe"
# A plain go build skips the resource step of the wails CLI. Without the
# manifest Windows treats the app as DPI-unaware and scales it as a bitmap.
syso="$app/rsrc_windows_amd64.syso"
trap 'rm -f "$syso"' EXIT
go run "github.com/akavel/rsrc@$PVM_RSRC_VERSION" -arch amd64 -manifest "$app/pvm-desktop.exe.manifest" -o "$syso"
# wv2runtime.browser: a missing WebView2 runtime opens Microsoft's download
# page; the default would download and install it without asking.
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -buildvcs=false \
  -tags desktop,production,wv2runtime.browser \
  -ldflags "-s -w -buildid= -H windowsgui -X github.com/huangzheng2016/pvm/internal/buildinfo.Version=${PVM_VERSION}" \
  -o pvm-desktop.exe ./cmd/pvm-desktop

# Without the production tag the build also succeeds, as a program with no
# frontend inside.
if ! LC_ALL=C grep -aqF "$(basename "$asset")" pvm-desktop.exe; then
  echo "error: pvm-desktop.exe does not embed the frontend ($(basename "$asset"))" >&2
  exit 1
fi
if ! LC_ALL=C grep -aqF dpiAwareness pvm-desktop.exe; then
  echo "error: pvm-desktop.exe has no application manifest" >&2
  exit 1
fi
echo "built pvm-desktop.exe ($PVM_VERSION)"

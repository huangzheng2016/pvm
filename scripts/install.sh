#!/bin/sh
set -e

OWNER="huangzheng2016"
REPO="pvm"
API="https://api.github.com/repos/${OWNER}/${REPO}/releases/latest"
BASE="https://github.com/${OWNER}/${REPO}/releases/download"

if [ -z "${TAG:-}" ]; then
  TAG=$(curl -fsSL "$API" | grep '"tag_name"' | head -1 | sed 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/')
fi
[ -n "$TAG" ] || { echo "could not read latest release tag" >&2; exit 1; }

S=$(uname -s)
M=$(uname -m)
case "$S/$M" in
  Darwin/arm64)        ASSET="pvm_darwin_arm64" ;;
  Linux/x86_64)        ASSET="pvm_linux_amd64" ;;
  Linux/aarch64|Linux/arm64)
    echo "linux/arm64 host is not supported; for a VPS exit-agent download pvm_linux_arm64.tar.gz from releases manually" >&2
    exit 1 ;;
  *) echo "unsupported host: $S/$M (supported: darwin/arm64, linux/amd64)" >&2; exit 1 ;;
esac

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT INT TERM
cd "$TMP"

echo "downloading ${ASSET}.tar.gz (${TAG})..."
curl -fsSL "$BASE/$TAG/${ASSET}.tar.gz" -o asset.tar.gz
curl -fsSL "$BASE/$TAG/SHA256SUMS" -o SHA256SUMS
grep "  ${ASSET}.tar.gz$" SHA256SUMS > want.sum || { echo "checksum entry missing" >&2; exit 1; }
if command -v shasum >/dev/null 2>&1; then
  awk '{print $1"  asset.tar.gz"}' want.sum | shasum -a 256 -c - >/dev/null
else
  awk '{print $1"  asset.tar.gz"}' want.sum | sha256sum -c - >/dev/null
fi

tar -xzf asset.tar.gz
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$INSTALL_DIR"
cp pvm "$INSTALL_DIR/pvm"
chmod +x "$INSTALL_DIR/pvm"
echo "installed: $INSTALL_DIR/pvm"

if [ "$S" = "Darwin" ] && [ "${PVM_SKIP_QEMU:-0}" != "1" ]; then
  echo "installing bundled qemu runtime (aarch64)..."
  curl -fsSL "$BASE/$TAG/pvm-qemu-runtime-aarch64.tar.gz" -o qemu.tar.gz
  grep "  pvm-qemu-runtime-aarch64.tar.gz$" SHA256SUMS > qemu.want.sum || { echo "qemu checksum entry missing" >&2; exit 1; }
  if command -v shasum >/dev/null 2>&1; then
    awk '{print $1"  qemu.tar.gz"}' qemu.want.sum | shasum -a 256 -c - >/dev/null
  else
    awk '{print $1"  qemu.tar.gz"}' qemu.want.sum | sha256sum -c - >/dev/null
  fi
  mkdir -p "$HOME/.pvm/runtime/qemu"
  tar -xzf qemu.tar.gz -C "$HOME/.pvm/runtime/qemu"
fi

case ":$PATH:" in
  *:"$INSTALL_DIR":*) ;;
  *) echo "add to PATH: export PATH=\"$INSTALL_DIR:\$PATH\"" ;;
esac

echo "next: pvm create --image ubuntu:26.04 --name dev1 --user dev  (首次会自动下载镜像并烘焙 golden，属一次性，约十几分钟)"

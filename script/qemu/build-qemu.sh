#!/bin/bash
# build-qemu.sh — pvm bundled QEMU recipe for the native host.
#
# QEMU >= 11.1.0 already carries the upstream darwin/arm64 HVF
# live-restore fix (06fd39e426), so no source patches are needed.
# ParavirtualizedGraphics is disabled: headless VMs don't need it and
# it fails to link against some macOS SDKs.
#
# Usage: QEMU_VERSION=11.1.2 ./script/qemu/build-qemu.sh <output-prefix>
set -euo pipefail

QEMU_VERSION="${QEMU_VERSION:-11.1.2}"
PREFIX="${1:?usage: build-qemu.sh <output-prefix>}"
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
. "$SCRIPT_DIR/../lib/native.sh"
pvm_native_target
case "$PVM_HOSTARCH" in
  arm64) TARGET=aarch64-softmmu; SYSTEM=qemu-system-aarch64 ;;
  amd64) TARGET=x86_64-softmmu; SYSTEM=qemu-system-x86_64 ;;
esac
SRC="${SRC_DIR:-/tmp/pvm-qemu-src}/qemu-${QEMU_VERSION}"

mkdir -p "$(dirname "$SRC")"
if [ ! -d "$SRC" ]; then
  tarball="/tmp/qemu-${QEMU_VERSION}.tar.xz"
  for url in \
    "https://download.qemu.org/qemu-${QEMU_VERSION}.tar.xz" \
    "https://mirrors.tuna.tsinghua.edu.cn/qemu/qemu-${QEMU_VERSION}.tar.xz"; do
    curl -sfL --connect-timeout 10 -o "$tarball" "$url" && break
  done
  tar -xJf "$tarball" -C "$(dirname "$SRC")"
fi

cd "$SRC"
if ! grep -q "pvm: headless builds" meson.build 2>/dev/null; then
  python3 - <<'PY'
p = 'meson.build'
s = open(p).read()
old = """  pvg = dependency('appleframeworks', modules: ['ParavirtualizedGraphics', 'Metal'],
                   required: get_option('pvg'))"""
if old in s:
    open(p, 'w').write(s.replace(old, "  pvg = not_found # pvm: headless builds skip ParavirtualizedGraphics"))
PY
fi

export PKG_CONFIG_PATH="/opt/homebrew/lib/pkgconfig:/opt/homebrew/share/pkgconfig:${PKG_CONFIG_PATH:-}"
[ -d build ] || ./configure \
  --target-list="$TARGET" \
  --disable-gtk --disable-sdl --disable-cocoa --disable-docs \
  --disable-guest-agent --disable-user \
  --enable-slirp --enable-capstone --enable-hvf \
  --prefix="$PREFIX"
cd build
CPUS=$(getconf _NPROCESSORS_ONLN 2>/dev/null || sysctl -n hw.ncpu)
ninja -j"$CPUS" "$SYSTEM" qemu-img
ninja install
echo "bundled qemu installed to $PREFIX"

#!/bin/bash
# build-qemu.sh — pvm bundled QEMU recipe (macOS arm64 first).
#
# Why not stock homebrew qemu: QEMU 11.0.0 has a darwin/arm64 HVF
# live-restore regression (cpu_pre_load assertion on incoming
# migration). The upstream fix 06fd39e426 first ships in 11.1.0, so we
# build 11.0.0 + that patch ourselves. ParavirtualizedGraphics is
# disabled: headless VMs don't need it and it fails to link against
# some macOS SDKs.
#
# Usage: QEMU_VERSION=11.0.0 ./script/qemu/build-qemu.sh <output-prefix>
set -euo pipefail

QEMU_VERSION="${QEMU_VERSION:-11.0.0}"
PREFIX="${1:?usage: build-qemu.sh <output-prefix>}"
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

cd "$(dirname "$0")"
cp hvf-fix.patch "$SRC"/../ || true
cd "$SRC"
if ! grep -q "pvm: headless builds" meson.build 2>/dev/null; then
  patch -p1 < ../hvf-fix.patch
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
  --target-list=aarch64-softmmu,x86_64-softmmu \
  --disable-gtk --disable-sdl --disable-cocoa --disable-docs \
  --disable-guest-agent --disable-user \
  --enable-slirp --enable-capstone --enable-hvf \
  --prefix="$PREFIX"
cd build
ninja -j"$(sysctl -n hw.ncpu)" qemu-system-aarch64 qemu-system-x86_64 qemu-img
ninja install
echo "bundled qemu installed to $PREFIX"

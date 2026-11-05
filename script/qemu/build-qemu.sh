#!/bin/bash
# build-qemu.sh — pvm bundled QEMU recipe for the native host.
#
# QEMU >= 11.1.0 already carries the upstream darwin/arm64 HVF
# live-restore fix (06fd39e426), so no source patches are needed.
# ParavirtualizedGraphics is disabled: headless VMs don't need it and
# it fails to link against some macOS SDKs.
#
# Usage: QEMU_VERSION=11.1.2 QEMU_SHA256=<verified-digest> ./script/qemu/build-qemu.sh <output-prefix>
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
# The expected digest must come from an independently verified upstream release.
: "${QEMU_SHA256:?set QEMU_SHA256 to the verified upstream archive SHA256}"
[[ "$QEMU_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "invalid QEMU version" >&2; exit 1; }
[[ "$QEMU_SHA256" =~ ^[0-9a-fA-F]{64}$ ]] || { echo "invalid QEMU SHA256" >&2; exit 1; }
WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/pvm-qemu-build.XXXXXX")
trap 'rm -rf "$WORK_DIR"' EXIT
SRC="$WORK_DIR/qemu-${QEMU_VERSION}"
tarball="$WORK_DIR/qemu.tar.xz"
curl --fail --show-error --location --proto '=https' --proto-redir '=https' --connect-timeout 10 \
  "https://download.qemu.org/qemu-${QEMU_VERSION}.tar.xz" -o "$tarball"
if command -v shasum >/dev/null 2>&1; then
  actual=$(shasum -a 256 "$tarball" | awk '{print $1}')
else
  actual=$(sha256sum "$tarball" | awk '{print $1}')
fi
[ "$actual" = "$(printf '%s' "$QEMU_SHA256" | tr 'A-F' 'a-f')" ] || { echo "QEMU archive checksum mismatch" >&2; exit 1; }
tar -xJf "$tarball" -C "$WORK_DIR"

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
ACCEL_ARGS=()
if [ "$PVM_HOSTOS" = darwin ]; then ACCEL_ARGS+=(--enable-hvf); else ACCEL_ARGS+=(--enable-kvm); fi
[ -d build ] || ./configure \
  --target-list="$TARGET" \
  --disable-gtk --disable-sdl --disable-cocoa --disable-docs \
  --disable-guest-agent --disable-user \
  --enable-slirp --enable-capstone "${ACCEL_ARGS[@]}" \
  --prefix="$PREFIX"
cd build
CPUS=$(getconf _NPROCESSORS_ONLN 2>/dev/null || sysctl -n hw.ncpu)
ninja -j"$CPUS" "$SYSTEM" qemu-img
ninja install
echo "bundled qemu installed to $PREFIX"

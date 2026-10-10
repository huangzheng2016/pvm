#!/bin/bash
# pack-runtime.sh — assemble pvm's bundled QEMU runtime from the local
# homebrew qemu bottle (no source build needed once stock QEMU carries
# the HVF live-restore fix, i.e. >= 11.1.0).
#
#   ./script/qemu/pack-runtime.sh [qemu-version] [dest]
#
# Defaults: version = brew's installed qemu, dest = ~/.pvm/runtime/qemu/aarch64.
# Writes SHA256SUMS alongside the binaries as the integrity record.
set -euo pipefail

VERSION="${1:-$(brew list --versions qemu | awk '{print $2}')}"
SRC="/opt/homebrew/Cellar/qemu/${VERSION}"
DEST="${2:-$HOME/.pvm/runtime/qemu/aarch64}"

[ -d "$SRC" ] || { echo "bottle not found: $SRC (brew install qemu)" >&2; exit 1; }

# Trim list: only what aarch64-virt / x86_64-q35 headless cloud VMs need.
BIN_KEEP="qemu-system-aarch64 qemu-system-x86_64 qemu-img"
FW_KEEP=(
  keymaps
  edk2-aarch64-code.fd
  edk2-x86_64-code.fd
  bios.bin bios-256k.bin kvmvapic.bin
  vgabios-stdvga.bin vgabios-virtio.bin
)
# NIC option ROMs are referenced even on headless probes; keep them all.
for f in "$SRC"/share/qemu/efi-*.rom "$SRC"/share/qemu/pxe-*.rom; do
  FW_KEEP+=("$(basename "$f")")
done

rm -rf "$DEST"
mkdir -p "$DEST/bin" "$DEST/share/qemu"
for b in $BIN_KEEP; do
  cp "$SRC/bin/$b" "$DEST/bin/"
done
for f in "${FW_KEEP[@]}"; do
  cp -R "$SRC/share/qemu/$f" "$DEST/share/qemu/" 2>/dev/null || echo "warn: $f missing in bottle" >&2
done

(
  cd "$DEST"
  shasum -a 256 bin/* > SHA256SUMS
  echo "$VERSION" > VERSION
)
echo "bundled qemu $VERSION -> $DEST"
du -sh "$DEST"

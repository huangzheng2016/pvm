#!/bin/bash
# pack-runtime.sh — assemble pvm's bundled QEMU runtime from the local
# homebrew qemu bottle (no source build needed once stock QEMU carries
# the HVF live-restore fix, i.e. >= 11.1.0).
#
#   ./script/qemu/pack-runtime.sh [qemu-version] [dest]
#
# Defaults: version = brew's installed qemu, dest = ~/.pvm/runtime/qemu/<native-qemu-arch>.
# Writes SHA256SUMS alongside the binaries as the integrity record.
set -euo pipefail

VERSION="${1:-$(brew list --versions qemu | awk '{print $2}')}"
SRC="/opt/homebrew/Cellar/qemu/${VERSION}"
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
. "$SCRIPT_DIR/../lib/native.sh"
pvm_native_target
# The bundled runtime ships only for darwin/arm64 (homebrew bottle layout,
# dylib rewrite, codesign); linux hosts use the system qemu instead.
[ "$PVM_HOSTOS/$PVM_HOSTARCH" = "darwin/arm64" ] || { echo "bundled runtime is darwin/arm64 only" >&2; exit 1; }
QARCH=aarch64; SYSTEM=qemu-system-aarch64; FW_ARCH=(edk2-aarch64-code.fd)
DEST="${2:-$HOME/.pvm/runtime/qemu/$QARCH}"

[ -d "$SRC" ] || { echo "bottle not found: $SRC (brew install qemu)" >&2; exit 1; }

# Trim list: only what the host architecture's headless cloud VMs need.
BIN_KEEP="$SYSTEM qemu-img"
FW_KEEP=(keymaps "${FW_ARCH[@]}")
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

# Self-contain the runtime: copy the homebrew dylib dependency closure into
# lib/ and rewrite references to bundle-relative paths (brew-less machines
# have no /opt/homebrew, which silently killed the previous bundle at dyld).
mkdir -p "$DEST/lib"
queue=("$DEST"/bin/*)
while [ ${#queue[@]} -gt 0 ]; do
  cur="${queue[0]}"; queue=("${queue[@]:1}")
  for dep in $(otool -L "$cur" 2>/dev/null | awk '{print $1}' | grep '^/opt/homebrew/.*\.dylib$' || true); do
    name=$(basename "$dep")
    [ -f "$DEST/lib/$name" ] || { cp "$dep" "$DEST/lib/$name"; chmod u+w "$DEST/lib/$name"; queue+=("$DEST/lib/$name"); }
  done
done
IDENTITY="${PVM_CODESIGN_IDENTITY:--}"
for f in "$DEST"/bin/* "$DEST"/lib/*.dylib; do
  [ -f "$f" ] || continue
  for dep in $(otool -L "$f" 2>/dev/null | awk '{print $1}' | grep '^/opt/homebrew/.*\.dylib$' || true); do
    install_name_tool -change "$dep" "@executable_path/../lib/$(basename "$dep")" "$f"
  done
  case "$f" in
    *.dylib) install_name_tool -id "@executable_path/../lib/$(basename "$f")" "$f" ;;
  esac
  # rpath rewrites invalidate the bottle's signature; re-sign or macOS
  # denies hvf. The entitlement is mandatory for qemu-system-* and a
  # signing failure must fail the pack (a silent one shipped once).
  case "$f" in
    */qemu-system-*) codesign -s "$IDENTITY" --force --entitlements "$SCRIPT_DIR/hv-entitlements.plist" "$f" ;;
    *) codesign -s "$IDENTITY" --force "$f" ;;
  esac
done

(
  cd "$DEST"
  shasum -a 256 bin/* > SHA256SUMS
  echo "$VERSION" > VERSION
)
echo "bundled qemu $VERSION -> $DEST"
du -sh "$DEST"

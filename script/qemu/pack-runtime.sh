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

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
. "$SCRIPT_DIR/../lib/native.sh"
pvm_native_target
[ "$PVM_HOSTOS/$PVM_HOSTARCH" = "darwin/arm64" ] || { echo "bundled runtime is darwin/arm64 only" >&2; exit 1; }
VERSION="${1:-$(brew list --versions qemu | awk '{print $2}')}"
if [[ ! "$VERSION" =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)$ ]] ||
   (( BASH_REMATCH[1] < 11 || (BASH_REMATCH[1] == 11 && BASH_REMATCH[2] < 1) )); then
  echo "error: bundled QEMU requires a release version >= 11.1.0" >&2
  exit 1
fi
SRC="/opt/homebrew/Cellar/qemu/${VERSION}"
QARCH=aarch64; SYSTEM=qemu-system-aarch64; FW_ARCH=(edk2-aarch64-code.fd)
DEST="${2:-$HOME/.pvm/runtime/qemu/$QARCH}"
if [ -e "$DEST" ] || [ -L "$DEST" ]; then
  if [ ! -L "$DEST" ] && [ -f "$DEST/VERSION" ] && [ "$(cat "$DEST/VERSION")" = "$VERSION" ] &&
     [ -f "$DEST/SHA256SUMS" ] && [ -f "$DEST/share/qemu/edk2-aarch64-code.fd" ] &&
     (cd "$DEST" && cmp -s SHA256SUMS <(find bin lib share -type f -print | LC_ALL=C sort | while IFS= read -r f; do shasum -a 256 "$f"; done)) &&
     "$DEST/bin/$SYSTEM" --version >/dev/null 2>&1 && "$DEST/bin/qemu-img" --version >/dev/null 2>&1 &&
     codesign --verify --strict "$DEST/bin/$SYSTEM" >/dev/null 2>&1 &&
     codesign -d --entitlements :- "$DEST/bin/$SYSTEM" 2>/dev/null | grep -q '<key>com.apple.security.hypervisor</key><true'; then
    echo "reusing verified bundled qemu $VERSION -> $DEST"
    exit 0
  fi
  echo "error: existing destination failed version, integrity or signature checks; choose a new directory" >&2
  exit 1
fi
parent=$(dirname "$DEST")
mkdir -p "$parent"
parent=$(cd "$parent" && pwd -P)
FINAL_DEST="$parent/$(basename "$DEST")"
DEST=$(mktemp -d "$parent/.pvm-qemu-pack.XXXXXX")
trap 'rm -rf "$DEST"' EXIT

[ -d "$SRC" ] || { echo "bottle not found: $SRC (brew install qemu)" >&2; exit 1; }

# Trim list: only what the host architecture's headless cloud VMs need.
BIN_KEEP="$SYSTEM qemu-img"
FW_KEEP=(keymaps "${FW_ARCH[@]}")
# NIC option ROMs are referenced even on headless probes; keep them all.
for f in "$SRC"/share/qemu/efi-*.rom "$SRC"/share/qemu/pxe-*.rom; do
  FW_KEEP+=("$(basename "$f")")
done

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
  find bin lib share -type f -print | LC_ALL=C sort | while IFS= read -r f; do shasum -a 256 "$f"; done > SHA256SUMS
  echo "$VERSION" > VERSION
)
"$DEST/bin/$SYSTEM" --version >/dev/null
"$DEST/bin/qemu-img" --version >/dev/null
codesign --verify --strict "$DEST/bin/$SYSTEM"
for f in "${FW_ARCH[@]}"; do
  [ -f "$DEST/share/qemu/$f" ] || { echo "error: firmware missing: $f" >&2; exit 1; }
done
if [ -e "$FINAL_DEST" ] || [ -L "$FINAL_DEST" ]; then
  echo "error: destination appeared during packing; refusing to overwrite it" >&2
  exit 1
fi
mv "$DEST" "$FINAL_DEST"
trap - EXIT
echo "bundled qemu $VERSION -> $FINAL_DEST"
du -sh "$FINAL_DEST"

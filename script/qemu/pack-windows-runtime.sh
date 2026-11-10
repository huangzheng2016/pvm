#!/usr/bin/env bash
# pack-windows-runtime.sh — assemble pvm's bundled Windows x86_64 QEMU runtime
# from the official w64 NSIS installer (qemu.weilnetz.de).
#
#   ./script/qemu/pack-windows-runtime.sh [dest] [installer-exe]
#
# Defaults: dest = ~/.pvm/runtime/qemu/x86_64
set -euo pipefail

DEST_ARG="${1:-$HOME/.pvm/runtime/qemu/x86_64}"
INSTALLER_ARG="${2:-${QEMU_WINDOWS_INSTALLER:-}}"
QEMU_WINDOWS_BASE_URL="${QEMU_WINDOWS_BASE_URL:-https://qemu.weilnetz.de/w64}"

sevenz=$(command -v 7zz || command -v 7z || command -v 7za || true)
[ -n "$sevenz" ] || { echo "error: 7z/7zz/7za is required to extract Windows QEMU installer" >&2; exit 1; }

resolve_windows_qemu_url() {
  if [ -n "${QEMU_WINDOWS_URL:-}" ]; then
    echo "$QEMU_WINDOWS_URL"
    return 0
  fi
  python3 - "$QEMU_WINDOWS_BASE_URL" <<'PY'
import datetime, re, sys, urllib.request
base = sys.argv[1].rstrip("/")
years = [datetime.date.today().year, datetime.date.today().year - 1, 2026, 2025]
seen = set()
for year in years:
    if year in seen:
        continue
    seen.add(year)
    url = f"{base}/{year}/"
    try:
        req = urllib.request.Request(url, headers={"User-Agent": "pvm-ci/1.0"})
        with urllib.request.urlopen(req, timeout=20) as r:
            html = r.read().decode("utf-8", "ignore")
    except Exception:
        continue
    names = sorted(set(re.findall(r"qemu-w64-setup-\d+\.exe", html)))
    if names:
        print(f"{url}{names[-1]}")
        sys.exit(0)
sys.exit(1)
PY
}

sha256_of() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    sha256sum "$1" | awk '{print $1}'
  fi
}

parent=$(dirname "$DEST_ARG")
mkdir -p "$parent"
parent=$(cd "$parent" && pwd -P)
FINAL_DEST="$parent/$(basename "$DEST_ARG")"
if [ -e "$FINAL_DEST" ] || [ -L "$FINAL_DEST" ]; then
  echo "error: destination $FINAL_DEST already exists; remove it or choose a new directory" >&2
  exit 1
fi

WORK=$(mktemp -d "$parent/.pvm-win-qemu-work.XXXXXX")
DEST=$(mktemp -d "$parent/.pvm-win-qemu-pack.XXXXXX")
trap 'rm -rf "$WORK" "$DEST"' EXIT

ARCHIVE="$INSTALLER_ARG"
SOURCE_URL=""
if [ -z "$ARCHIVE" ]; then
  SOURCE_URL=$(resolve_windows_qemu_url) || { echo "error: failed to resolve Windows QEMU installer URL" >&2; exit 1; }
  ARCHIVE="$WORK/$(basename "$SOURCE_URL")"
  echo "downloading Windows QEMU from $SOURCE_URL..."
  curl --fail --show-error --location --proto '=https' --proto-redir '=https' --connect-timeout 20 --retry 3 \
    "$SOURCE_URL" -o "$ARCHIVE"
fi
[ -f "$ARCHIVE" ] || { echo "error: installer not found: $ARCHIVE" >&2; exit 1; }

VERSION=$(basename "$ARCHIVE" | sed -E 's/^qemu-w64-setup-([0-9]+)\.exe$/\1/')
[ -n "$VERSION" ] || VERSION="w64"
INSTALLER_SHA256=$(sha256_of "$ARCHIVE")

EXTRACT="$WORK/extract"
mkdir -p "$EXTRACT"
"$sevenz" x -y "-o$EXTRACT" "$ARCHIVE" >/dev/null

IMG_BIN=$(find "$EXTRACT" -type f -iname "qemu-img.exe" -print -quit)
[ -n "$IMG_BIN" ] || { echo "error: qemu-img.exe not found in installer" >&2; exit 1; }
BINDIR=$(dirname "$IMG_BIN")
[ -f "$BINDIR/qemu-system-x86_64.exe" ] || { echo "error: qemu-system-x86_64.exe not found in installer" >&2; exit 1; }

mkdir -p "$DEST/bin/share"
cp "$BINDIR/qemu-img.exe" "$BINDIR/qemu-system-x86_64.exe" "$DEST/bin/"
for dll in "$BINDIR"/*.dll; do
  [ -f "$dll" ] && cp "$dll" "$DEST/bin/"
done

SHAREDIR=""
if [ -d "$BINDIR/share" ]; then
  SHAREDIR="$BINDIR/share"
elif [ -d "$BINDIR/../share/qemu" ]; then
  SHAREDIR="$BINDIR/../share/qemu"
elif [ -d "$BINDIR/../share" ]; then
  SHAREDIR="$BINDIR/../share"
fi
if [ -n "$SHAREDIR" ]; then
  [ -d "$SHAREDIR/keymaps" ] && cp -R "$SHAREDIR/keymaps" "$DEST/bin/share/"
  for pat in "bios*.bin" "q35-acpi-dsdt.aml" "kvmvapic.bin" "linuxboot*.bin" "multiboot*.bin" "vgabios*.bin" "efi-*.rom" "pxe-*.rom" "edk2-x86_64*.fd" "edk2-i386-vars.fd"; do
    for f in "$SHAREDIR"/$pat; do
      [ -f "$f" ] && cp "$f" "$DEST/bin/share/"
    done
  done
fi

[ -f "$DEST/bin/share/bios-256k.bin" ] || {
  echo "error: Windows QEMU SeaBIOS firmware missing in extracted bundle" >&2
  exit 1
}

(
  cd "$DEST"
  find bin -type f -print | LC_ALL=C sort | while IFS= read -r f; do
    if command -v shasum >/dev/null 2>&1; then
      shasum -a 256 "$f"
    else
      sha256sum "$f"
    fi
  done > SHA256SUMS
  echo "$VERSION" > VERSION
)

if [ -n "${QEMU_METADATA_JSON:-}" ]; then
  python3 - "$QEMU_METADATA_JSON" "$VERSION" "${SOURCE_URL:-local}" "$INSTALLER_SHA256" <<'PY'
import json, sys
out, ver, url, sha = sys.argv[1:5]
with open(out, "w", encoding="utf-8") as f:
    json.dump({"version": ver, "url": url, "installer_sha256": sha}, f, indent=2)
    f.write("\n")
PY
fi

if [ -e "$FINAL_DEST" ] || [ -L "$FINAL_DEST" ]; then
  echo "error: destination $FINAL_DEST appeared while packing; leaving it unchanged" >&2
  exit 1
fi
mv "$DEST" "$FINAL_DEST"
rm -rf "$WORK"
trap - EXIT
echo "bundled windows qemu $VERSION -> $FINAL_DEST"
du -sh "$FINAL_DEST"

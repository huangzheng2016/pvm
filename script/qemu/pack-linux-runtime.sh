#!/usr/bin/env bash
# pack-linux-runtime.sh — assemble a relocatable Linux x86_64 QEMU runtime.
#
#   ./script/qemu/pack-linux-runtime.sh [dest]
#
# Defaults: dest = ~/.pvm/runtime/qemu/x86_64
set -euo pipefail

DEST_ARG="${1:-$HOME/.pvm/runtime/qemu/x86_64}"
SRC_SYSTEM="${QEMU_LINUX_SYSTEM:-/usr/bin/qemu-system-x86_64}"
SRC_IMG="${QEMU_LINUX_IMG:-/usr/bin/qemu-img}"
SRC_SHARE="${QEMU_LINUX_SHARE:-}"

[ -x "$SRC_SYSTEM" ] || { echo "error: $SRC_SYSTEM not found; install qemu-system-x86" >&2; exit 1; }
[ -x "$SRC_IMG" ] || { echo "error: $SRC_IMG not found; install qemu-utils" >&2; exit 1; }
if [ -z "$SRC_SHARE" ]; then
  for candidate in /usr/share/qemu /usr/share/kvm /usr/share/seabios; do
    if [ -f "$candidate/bios-256k.bin" ]; then
      SRC_SHARE="$candidate"
      break
    fi
  done
fi
[ -d "$SRC_SHARE" ] || { echo "error: QEMU data directory not found; set QEMU_LINUX_SHARE" >&2; exit 1; }
command -v ldd >/dev/null 2>&1 || { echo "error: ldd is required" >&2; exit 1; }
command -v patchelf >/dev/null 2>&1 || { echo "error: patchelf is required" >&2; exit 1; }

parent=$(dirname "$DEST_ARG")
mkdir -p "$parent"
parent=$(cd "$parent" && pwd -P)
FINAL_DEST="$parent/$(basename "$DEST_ARG")"
if [ -e "$FINAL_DEST" ] || [ -L "$FINAL_DEST" ]; then
  echo "error: destination $FINAL_DEST already exists; remove it or choose a new directory" >&2
  exit 1
fi
DEST=$(mktemp -d "$parent/.pvm-linux-qemu-pack.XXXXXX")
trap 'rm -rf "$DEST"' EXIT

mkdir -p "$DEST/bin" "$DEST/lib" "$DEST/share/qemu"
cp "$SRC_SYSTEM" "$DEST/bin/qemu-system-x86_64.real"
cp "$SRC_IMG" "$DEST/bin/qemu-img.real"
cp -RL "$SRC_SHARE/." "$DEST/share/qemu/"

declare -A seen=()
queue=("$DEST/bin/qemu-system-x86_64.real" "$DEST/bin/qemu-img.real")
while [ ${#queue[@]} -gt 0 ]; do
  cur="${queue[0]}"
  queue=("${queue[@]:1}")
  while IFS= read -r dep; do
    [ -n "$dep" ] || continue
    [ -f "$dep" ] || continue
    name=$(basename "$dep")
    if [ -z "${seen[$name]:-}" ]; then
      cp "$dep" "$DEST/lib/$name"
      seen[$name]=1
      queue+=("$DEST/lib/$name")
    fi
  done < <(ldd "$cur" 2>/dev/null | awk '/=> \// {print $3} /^\// {print $1}')
done
loader=$(patchelf --print-interpreter "$SRC_SYSTEM")
[ -f "$loader" ] || { echo "error: could not determine ELF interpreter" >&2; exit 1; }
LOADER_NAME=$(basename "$loader")
cp "$loader" "$DEST/lib/$LOADER_NAME"

patchelf --set-rpath '$ORIGIN/../lib' "$DEST/bin/qemu-system-x86_64.real" "$DEST/bin/qemu-img.real"
for name in qemu-system-x86_64 qemu-img; do
  if [ "$name" = "qemu-system-x86_64" ]; then
    cat > "$DEST/bin/$name" <<EOF
#!/bin/sh
here=\$(CDPATH= cd -- "\$(dirname -- "\$0")" && pwd)
exec "\$here/../lib/$LOADER_NAME" --library-path "\$here/../lib" "\$here/$name.real" -L "\$here/../share/qemu" "\$@"
EOF
  else
    cat > "$DEST/bin/$name" <<EOF
#!/bin/sh
here=\$(CDPATH= cd -- "\$(dirname -- "\$0")" && pwd)
exec "\$here/../lib/$LOADER_NAME" --library-path "\$here/../lib" "\$here/$name.real" "\$@"
EOF
  fi
  chmod 0755 "$DEST/bin/$name"
done

[ -f "$DEST/share/qemu/bios-256k.bin" ] || { echo "error: bios-256k.bin missing from QEMU data" >&2; exit 1; }
VERSION=$("$SRC_SYSTEM" --version | sed -nE '1s/^QEMU emulator version ([0-9]+\.[0-9]+\.[0-9]+).*$/\1/p')
[ -n "$VERSION" ] || { echo "error: cannot determine QEMU version" >&2; exit 1; }
echo "$VERSION" > "$DEST/VERSION"
(
  cd "$DEST"
  find bin lib share -type f -print | LC_ALL=C sort | while IFS= read -r f; do sha256sum "$f"; done > SHA256SUMS
)

"$DEST/bin/qemu-system-x86_64" --version >/dev/null
"$DEST/bin/qemu-img" --version >/dev/null
"$DEST/bin/qemu-system-x86_64" -machine q35,accel=tcg -display none -S >"$DEST/probe.log" 2>&1 &
pid=$!
sleep 1
if kill -0 "$pid" 2>/dev/null; then
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  rm -f "$DEST/probe.log"
else
  wait "$pid" 2>/dev/null || {
    cat "$DEST/probe.log" >&2
    echo "error: bundled QEMU failed its BIOS/startup probe" >&2
    exit 1
  }
fi

if [ -n "${QEMU_METADATA_JSON:-}" ]; then
  python3 - "$QEMU_METADATA_JSON" "$VERSION" "$SRC_SYSTEM" <<'PY'
import json, sys
out, ver, source = sys.argv[1:4]
with open(out, "w", encoding="utf-8") as f:
    json.dump({"version": ver, "source": source}, f, indent=2)
    f.write("\n")
PY
fi

if [ -e "$FINAL_DEST" ] || [ -L "$FINAL_DEST" ]; then
  echo "error: destination $FINAL_DEST appeared while packing; leaving it unchanged" >&2
  exit 1
fi
mv "$DEST" "$FINAL_DEST"
trap - EXIT
echo "bundled linux qemu $VERSION -> $FINAL_DEST"
du -sh "$FINAL_DEST"

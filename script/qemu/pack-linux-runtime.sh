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
SRC_SHARE="${QEMU_LINUX_SHARE:-/usr/share/qemu}"
SRC_BIOS="${QEMU_LINUX_BIOS:-/usr/share/seabios}"
SRC_IPXE="${QEMU_LINUX_IPXE:-/usr/lib/ipxe/qemu}"
SRC_MODULES="${QEMU_LINUX_MODULES:-/usr/lib/x86_64-linux-gnu/qemu}"

[ -x "$SRC_SYSTEM" ] || { echo "error: $SRC_SYSTEM not found; install qemu-system-x86" >&2; exit 1; }
[ -x "$SRC_IMG" ] || { echo "error: $SRC_IMG not found; install qemu-utils" >&2; exit 1; }
[ -d "$SRC_SHARE" ] || { echo "error: QEMU data directory not found: $SRC_SHARE" >&2; exit 1; }
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

mkdir -p "$DEST/bin" "$DEST/lib/qemu" "$DEST/share/qemu"
cp "$SRC_SYSTEM" "$DEST/bin/qemu-system-x86_64.real"
cp "$SRC_IMG" "$DEST/bin/qemu-img.real"
for data_dir in "$SRC_SHARE" "$SRC_BIOS" "$SRC_IPXE"; do
  if [ -d "$data_dir" ]; then
    cp -RL "$data_dir/." "$DEST/share/qemu/"
  fi
done
if [ -d "$SRC_MODULES" ]; then
  cp -RL "$SRC_MODULES/." "$DEST/lib/qemu/"
fi

declare -A seen=()
queue=("$DEST/bin/qemu-system-x86_64.real" "$DEST/bin/qemu-img.real")
if [ -d "$DEST/lib/qemu" ]; then
  while IFS= read -r -d '' module; do
    queue+=("$module")
  done < <(find "$DEST/lib/qemu" -type f -print0)
fi
while [ ${#queue[@]} -gt 0 ]; do
  cur="${queue[0]}"
  queue=("${queue[@]:1}")
  if ! ldd "$cur" 2>/dev/null | awk '/not found/{bad=1} END{exit bad}'; then
    echo "error: unresolved shared library dependency in $cur" >&2
    ldd "$cur" >&2 || true
    exit 1
  fi
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
if [ -d "$DEST/lib/qemu" ]; then
  find "$DEST/lib/qemu" -type f -exec patchelf --set-rpath '$ORIGIN/..' {} +
fi
for name in qemu-system-x86_64 qemu-img; do
  if [ "$name" = "qemu-system-x86_64" ]; then
    cat > "$DEST/bin/$name" <<EOF
#!/bin/sh
here=\$(CDPATH= cd -- "\$(dirname -- "\$0")" && pwd)
export QEMU_MODULE_DIR="\$here/../lib/qemu"
exec "\$here/../lib/$LOADER_NAME" --library-path "\$here/../lib" "\$here/$name.real" -L "\$here/../share/qemu" "\$@"
EOF
  else
    cat > "$DEST/bin/$name" <<EOF
#!/bin/sh
here=\$(CDPATH= cd -- "\$(dirname -- "\$0")" && pwd)
export QEMU_MODULE_DIR="\$here/../lib/qemu"
exec "\$here/../lib/$LOADER_NAME" --library-path "\$here/../lib" "\$here/$name.real" "\$@"
EOF
  fi
  chmod 0755 "$DEST/bin/$name"
done

for required in bios-256k.bin kvmvapic.bin efi-virtio.rom; do
  [ -s "$DEST/share/qemu/$required" ] || { echo "error: required firmware $required missing from QEMU data" >&2; exit 1; }
done
VERSION=$("$SRC_SYSTEM" --version | sed -nE '1s/^QEMU emulator version ([0-9]+\.[0-9]+\.[0-9]+).*$/\1/p')
[ -n "$VERSION" ] || { echo "error: cannot determine QEMU version" >&2; exit 1; }
echo "$VERSION" > "$DEST/VERSION"
(
  cd "$DEST"
  find bin lib share -type f -print | LC_ALL=C sort | while IFS= read -r f; do sha256sum "$f"; done > SHA256SUMS
)

"$DEST/bin/qemu-system-x86_64" --version >/dev/null
"$DEST/bin/qemu-img" --version >/dev/null
probe_args=(-machine q35,accel=tcg -display none -netdev user,id=probe0 -device virtio-net-pci,netdev=probe0 -device virtio-gpu-pci -S)
if [ "$(id -u)" -eq 0 ]; then
  command -v unshare >/dev/null 2>&1 || { echo "error: unshare is required for the isolated runtime probe" >&2; exit 1; }
  unshare --mount --propagation private bash -c '
    set -e
    for dir in /usr/share/qemu /usr/share/seabios /usr/lib/ipxe/qemu /usr/lib/x86_64-linux-gnu/qemu; do
      if [ -d "$dir" ]; then mount -t tmpfs tmpfs "$dir"; fi
    done
    exec "$@"
  ' _ "$DEST/bin/qemu-system-x86_64" "${probe_args[@]}" >"$DEST/probe.log" 2>&1 &
else
  "$DEST/bin/qemu-system-x86_64" "${probe_args[@]}" >"$DEST/probe.log" 2>&1 &
fi
pid=$!
sleep 2
if kill -0 "$pid" 2>/dev/null; then
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  rm -f "$DEST/probe.log"
else
  wait "$pid" 2>/dev/null || true
  cat "$DEST/probe.log" >&2
  echo "error: bundled QEMU failed its isolated firmware, module and startup probe" >&2
  exit 1
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

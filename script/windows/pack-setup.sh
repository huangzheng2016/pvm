#!/usr/bin/env bash
# pack-setup.sh: build the per-user Windows installers from a built pvm.exe, an
# assembled Windows QEMU runtime (script/qemu/pack-windows-runtime.sh) and the
# Xpra client for `pvm app` (script/windows/fetch-xpra-client.sh):
# <out>.exe (NSIS) and <out>.msi (wixl).
#
#   ./script/windows/pack-setup.sh <pvm.exe> <runtime-x86_64-dir> <xpra-dir> [out-basename]
#
# Needs makensis (NSIS 3) for exe and wixl/wixl-heat (msitools) plus python3
# for msi; on Ubuntu: apt install nsis wixl. PVM_SETUP_FORMATS selects a subset, e.g.
# "exe" on a Windows host where wixl is unavailable.
set -euo pipefail
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)

[ $# -ge 3 ] || { echo "usage: $0 <pvm.exe> <runtime-x86_64-dir> <xpra-dir> [out-basename]" >&2; exit 2; }
abs() { (cd "$(dirname "$1")" && printf '%s/%s\n' "$(pwd)" "$(basename "$1")"); }
PVM_EXE=$(abs "$1")
RUNTIME_DIR=$(cd "$2" && pwd)
XPRA_DIR=$(cd "$3" && pwd)
OUT=$(abs "${4:-pvm_windows_amd64_setup}")
FORMATS=${PVM_SETUP_FORMATS:-exe msi}
# tr: a CRLF checkout (core.autocrlf) would otherwise leave \r in the version.
VERSION=$(printf %s "${PVM_VERSION:-$(cat "$SCRIPT_DIR/../../VERSION")}" | tr -d '\r\n')
# MSI ProductVersion is numeric major.minor.build.
MSI_VERSION=${VERSION#v}
MSI_VERSION=${MSI_VERSION%%-*}
case "$MSI_VERSION" in
  *[!0-9.]*|.*|*.|*..*) echo "error: cannot derive an MSI version from $VERSION" >&2; exit 1 ;;
esac

[ -f "$PVM_EXE" ] || { echo "error: $PVM_EXE not found" >&2; exit 1; }
for f in bin/qemu-system-x86_64.exe bin/qemu-img.exe bin/share/bios-256k.bin; do
  [ -f "$RUNTIME_DIR/$f" ] || { echo "error: runtime is missing $f" >&2; exit 1; }
done
for f in Xpra.exe Xpra_cmd.exe share/xpra/COPYING XPRA-SOURCE.txt; do
  [ -f "$XPRA_DIR/$f" ] || { echo "error: xpra client is missing $f" >&2; exit 1; }
done

# makensis on Windows needs native paths; on Linux they pass through.
native() {
  if command -v cygpath >/dev/null 2>&1; then cygpath -w "$1"; else printf '%s\n' "$1"; fi
}

build_exe() {
  local makensis
  makensis=$(command -v makensis || true)
  if [ -z "$makensis" ] && [ -x "/c/Program Files (x86)/NSIS/Bin/makensis.exe" ]; then
    makensis="/c/Program Files (x86)/NSIS/Bin/makensis.exe"
  fi
  [ -n "$makensis" ] || { echo "error: makensis (NSIS 3) is required" >&2; exit 1; }
  "$makensis" -V2 -WX -INPUTCHARSET UTF8 \
    "-DVERSION=$VERSION" \
    "-DPVM_EXE=$(native "$PVM_EXE")" \
    "-DRUNTIME_DIR=$(native "$RUNTIME_DIR")" \
    "-DXPRA_DIR=$(native "$XPRA_DIR")" \
    "-DOUTFILE=$(native "$OUT.exe")" \
    "$(native "$SCRIPT_DIR/pvm-setup.nsi")"
  echo "built $OUT.exe"
}

build_msi() {
  command -v wixl >/dev/null 2>&1 && command -v wixl-heat >/dev/null 2>&1 \
    || { echo "error: wixl and wixl-heat (msitools) are required" >&2; exit 1; }
  local work
  work=$(mktemp -d)
  # shellcheck disable=SC2064
  trap "rm -rf '$work'" RETURN
  heat "$RUNTIME_DIR" RUNTIMEDIR RuntimeFiles RuntimeDir runtime/x86_64/ > "$work/runtime.wxs"
  heat "$XPRA_DIR" XPRADIR XpraFiles XpraDir xpra/ > "$work/xpra.wxs"
  local running_b64
  running_b64=$(base64 < "$SCRIPT_DIR/pvm-running.ps1" | tr -d '\r\n')
  wixl -a x64 \
    -D "Version=$VERSION" -D "MsiVersion=$MSI_VERSION" -D Win64=yes \
    -D "RunningPs1B64=$running_b64" \
    -D "PvmExe=$PVM_EXE" -D "RuntimeDir=$RUNTIME_DIR" -D "XpraDir=$XPRA_DIR" -D "UserPathPs1=$SCRIPT_DIR/user-path.ps1" \
    -o "$OUT.msi" "$SCRIPT_DIR/pvm-setup.wxs" "$work/runtime.wxs" "$work/xpra.wxs"
  echo "built $OUT.msi"
}

# heat <dir> <directory-ref> <component-group> <var> <guid-prefix> prints a
# component group with every file under dir.
heat() {
  local dir=$1
  (cd "$dir" && find . -type f | sed 's|^\./||' | LC_ALL=C sort) \
    | sed "s|^|$dir/|" \
    | wixl-heat -p "$dir/" --directory-ref "$2" --component-group "$3" --var "var.$4" --win64 \
    | stable_guids "$4" "$5"
}

# wixl-heat's Guid="*" is random per build. A major upgrade then sees each
# file as another component, skips it as already present, and the old
# product's removal deletes it. Derive the GUID from the file path.
stable_guids() {
  python3 -c '
import re, sys, uuid
var, prefix = sys.argv[1], sys.argv[2]
ns = uuid.UUID("6b0e7b6c-3c1f-4e36-9b4e-2e5a9e0d6f31")
src = sys.stdin.read()
pat = re.compile(r"Guid=\"\*\">(\s*<File [^>]*Source=\"\$\(var\." + var + r"\)/([^\"]+)\")")
out, n = pat.subn(lambda m: "Guid=\"{%s}\">%s" % (str(uuid.uuid5(ns, prefix + m.group(2).lower())).upper(), m.group(1)), src)
if n == 0 or "Guid=\"*\"" in out:
    sys.exit("error: could not assign stable component GUIDs")
sys.stdout.write(out)
' "$1" "$2"
}

for fmt in $FORMATS; do
  case "$fmt" in
    exe) build_exe ;;
    msi) build_msi ;;
    *) echo "error: unknown setup format $fmt (want exe or msi)" >&2; exit 2 ;;
  esac
done

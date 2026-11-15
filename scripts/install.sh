#!/bin/sh
set -eu
umask 077

OWNER="huangzheng2016"
REPO="pvm"
API="https://api.github.com/repos/${OWNER}/${REPO}/releases/latest"
BASE="https://github.com/${OWNER}/${REPO}/releases/download"
command -v gh >/dev/null 2>&1 || { echo "GitHub CLI (gh) with attestation support is required; install it before running this installer" >&2; exit 1; }
attest() {
  gh attestation verify "$1" --repo "$OWNER/$REPO" --signer-workflow "$OWNER/$REPO/.github/workflows/build.yml" --source-ref "refs/tags/$TAG" --deny-self-hosted-runners --hostname github.com || { echo "release attestation verification failed; installed version unchanged" >&2; exit 1; }
}

if [ -z "${TAG:-}" ]; then
  TAG=$(curl -fsSL "$API" | grep '"tag_name"' | head -1 | sed 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/')
fi
case "$TAG" in
  ''|*[!a-zA-Z0-9.-]*|.*|-*) echo "invalid release tag" >&2; exit 1 ;;
esac
S=$(uname -s)
M=$(uname -m)
BIN_NAME=pvm
QEMU_ARCH=""
QEMU_ASSET=""
FIRMWARE_REL=""
case "$S/$M" in
  Darwin/arm64)
    ASSET="pvm_darwin_arm64"
    QEMU_ARCH="aarch64"
    QEMU_ASSET="pvm-qemu-runtime-aarch64.tar.gz"
    FIRMWARE_REL="share/qemu/edk2-aarch64-code.fd"
    ;;
  Linux/x86_64)
    ASSET="pvm_linux_amd64"
    QEMU_ARCH="x86_64"
    QEMU_ASSET="pvm-qemu-runtime-linux-x86_64.tar.gz"
    FIRMWARE_REL="share/qemu/bios-256k.bin"
    ;;
  MINGW64_NT-*/x86_64|MSYS_NT-*/x86_64)
    ASSET="pvm_windows_amd64"
    BIN_NAME="pvm.exe"
    QEMU_ARCH="x86_64"
    QEMU_ASSET="pvm-qemu-runtime-windows-x86_64.tar.gz"
    FIRMWARE_REL="bin/share/bios-256k.bin"
    ;;
  Linux/aarch64|Linux/arm64)
    echo "linux/arm64 host is not supported; for a VPS exit-agent download pvm_linux_arm64.tar.gz from releases manually" >&2; exit 1 ;;
  *) echo "unsupported host: $S/$M (supported: darwin/arm64, linux/amd64, windows/amd64)" >&2; exit 1 ;;
esac

INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$INSTALL_DIR"
INSTALL_DIR=$(cd "$INSTALL_DIR" && pwd)
# Staging on the destination filesystem makes the final rename atomic.
LOCK_DIR="$INSTALL_DIR/.pvm-install.lock"
mkdir "$LOCK_DIR" 2>/dev/null || { echo "another installation is active (or interrupted; inspect $LOCK_DIR)" >&2; exit 1; }
TMP=$(mktemp -d "$INSTALL_DIR/.pvm-install.XXXXXX") || { rmdir "$LOCK_DIR"; exit 1; }
case "$S" in
  MINGW64_NT-*|MSYS_NT-*)
    if [ -z "${PVM_HOME:-}" ] && [ -n "${USERPROFILE:-}" ] && command -v cygpath >/dev/null 2>&1; then
      PVM_HOME="$(cygpath -u "$USERPROFILE")/.pvm"
      export PVM_HOME
    fi
    ;;
esac
RUNTIME_ROOT="${PVM_HOME:-$HOME/.pvm}/runtime/qemu"
RUNTIME_STAGE=""
RUNTIME_OLD=""
RUNTIME_SWITCHED=0
BINARY_SWITCHED=0
BINARY_BACKUP=""
COMMITTED=0
cleanup() {
  rc=$?
  trap - EXIT INT TERM
  if [ "$COMMITTED" = 0 ] && [ "$RUNTIME_SWITCHED" = 1 ]; then
    rm -rf "$RUNTIME_ROOT/$QEMU_ARCH"
    if [ -n "$RUNTIME_OLD" ]; then
      mv "$RUNTIME_OLD" "$RUNTIME_ROOT/$QEMU_ARCH" || echo "runtime restore failed; restore $RUNTIME_OLD to $RUNTIME_ROOT/$QEMU_ARCH" >&2
    fi
  fi
  if [ "$COMMITTED" = 0 ] && [ "$BINARY_SWITCHED" = 1 ]; then
    if [ -n "$BINARY_BACKUP" ]; then
      cp -p "$BINARY_BACKUP" "$INSTALL_DIR/$BIN_NAME"
    else
      rm -f "$INSTALL_DIR/$BIN_NAME"
    fi
  fi
  if [ "$COMMITTED" = 0 ] && [ -n "$RUNTIME_STAGE" ]; then rm -rf "$RUNTIME_STAGE"; fi
  rm -rf "$TMP"
  rmdir "$LOCK_DIR"
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
cd "$TMP"

verify() {
  name=$1
  file=$2
  awk -v name="$name" '$2 == name {print $1 "  " file; n++} END {if (n != 1) exit 1}' file="$file" SHA256SUMS > want.sum || { echo "missing or duplicate checksum entry: $name" >&2; exit 1; }
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 -c want.sum >/dev/null
  else
    sha256sum -c want.sum >/dev/null
  fi
}

echo "downloading ${ASSET}.tar.gz (${TAG})..."
curl -fsSL "$BASE/$TAG/${ASSET}.tar.gz" -o asset.tar.gz
curl -fsSL "$BASE/$TAG/SHA256SUMS" -o SHA256SUMS
attest SHA256SUMS
attest asset.tar.gz
verify "${ASSET}.tar.gz" asset.tar.gz
# Extract only the executable, never arbitrary archive paths.
[ "$(tar -tzf asset.tar.gz)" = "$BIN_NAME" ] || { echo "unexpected binary archive entries" >&2; exit 1; }
tar -tvzf asset.tar.gz | awk 'substr($0,1,1) != "-" {bad=1} END {exit bad}' || { echo "binary must be a regular file" >&2; exit 1; }
tar -xzf asset.tar.gz "$BIN_NAME"
[ -f "$BIN_NAME" ] && [ ! -L "$BIN_NAME" ] || { echo "invalid executable archive entry" >&2; exit 1; }
command -v file >/dev/null 2>&1 || { echo "file utility is required to validate the release platform" >&2; exit 1; }
BINARY_FORMAT=$(file -b "$BIN_NAME")
case "$S/$BINARY_FORMAT" in
  Darwin/*Mach-O*arm64*|Linux/*ELF*64-bit*x86-64*|MINGW64_NT-*/PE32+*|MSYS_NT-*/PE32+*) ;;
  *) echo "release platform mismatch" >&2; exit 1 ;;
esac
chmod 755 "$BIN_NAME"
[ "$(./$BIN_NAME version)" = "$TAG" ] || { echo "release version mismatch" >&2; exit 1; }

if [ -n "$QEMU_ASSET" ] && [ "${PVM_SKIP_QEMU:-0}" != "1" ]; then
  echo "staging bundled qemu runtime ($QEMU_ARCH)..."
  curl -fsSL "$BASE/$TAG/$QEMU_ASSET" -o qemu.tar.gz
  attest qemu.tar.gz
  verify "$QEMU_ASSET" qemu.tar.gz
  tar -tzf qemu.tar.gz > qemu.entries
  awk -v arch="$QEMU_ARCH" '/^\// || /(^|\/)\.\.(\/|$)/ {bad=1} $0 != arch && index($0, arch"/") != 1 {bad=1} END {exit bad}' qemu.entries || { echo "unsafe runtime archive paths" >&2; exit 1; }
  mkdir -p "$RUNTIME_ROOT"
  RUNTIME_ROOT=$(cd "$RUNTIME_ROOT" && pwd)
  RUNTIME_STAGE=$(mktemp -d "$RUNTIME_ROOT/release-${TAG}.XXXXXX")
  tar -tvzf qemu.tar.gz | awk 'substr($0,1,1) != "-" && substr($0,1,1) != "d" {bad=1} END {exit bad}' || { echo "runtime archive must contain only files and directories" >&2; exit 1; }
  tar -xzf qemu.tar.gz -C "$RUNTIME_STAGE"
  [ -x "$RUNTIME_STAGE/$QEMU_ARCH/bin/qemu-system-$QEMU_ARCH" ] || { echo "runtime executable missing" >&2; exit 1; }
  "$RUNTIME_STAGE/$QEMU_ARCH/bin/qemu-system-$QEMU_ARCH" --version >/dev/null
  "$RUNTIME_STAGE/$QEMU_ARCH/bin/qemu-img" --version >/dev/null
  [ -s "$RUNTIME_STAGE/$QEMU_ARCH/$FIRMWARE_REL" ] || { echo "runtime firmware missing" >&2; exit 1; }
  if [ "$S" = "Darwin" ]; then
    codesign --verify --strict "$RUNTIME_STAGE/$QEMU_ARCH/bin/qemu-system-$QEMU_ARCH"
    codesign -d --entitlements :- "$RUNTIME_STAGE/$QEMU_ARCH/bin/qemu-system-$QEMU_ARCH" 2>/dev/null | grep -q '<key>com.apple.security.hypervisor</key><true' || { echo "runtime HVF entitlement missing" >&2; exit 1; }
  fi
fi

# Preserve the previous binary before changing either installed component.
if [ -e "$INSTALL_DIR/$BIN_NAME" ]; then
  BACKUP=$(mktemp "$INSTALL_DIR/$BIN_NAME.previous.XXXXXX")
  cp -p "$INSTALL_DIR/$BIN_NAME" "$BACKUP"
  BINARY_BACKUP="$BACKUP"
  echo "binary rollback: cp '$BACKUP' '$INSTALL_DIR/.pvm-rollback' && mv -f '$INSTALL_DIR/.pvm-rollback' '$INSTALL_DIR/$BIN_NAME'"
fi
if [ -n "$RUNTIME_STAGE" ]; then
  RUNTIME_SWITCHED=1
  if [ -e "$RUNTIME_ROOT/$QEMU_ARCH" ] || [ -L "$RUNTIME_ROOT/$QEMU_ARCH" ]; then
    RUNTIME_OLD="$RUNTIME_STAGE/previous"
    if [ -L "$RUNTIME_ROOT/$QEMU_ARCH" ]; then
      cp -P "$RUNTIME_ROOT/$QEMU_ARCH" "$RUNTIME_OLD"
    else
      mv "$RUNTIME_ROOT/$QEMU_ARCH" "$RUNTIME_OLD"
    fi
  fi
  if [ "$S" = "Darwin" ]; then
    ln -s "$RUNTIME_STAGE/$QEMU_ARCH" "$RUNTIME_STAGE/current"
    mv -fh "$RUNTIME_STAGE/current" "$RUNTIME_ROOT/$QEMU_ARCH"
  else
    mv -f "$RUNTIME_STAGE/$QEMU_ARCH" "$RUNTIME_ROOT/$QEMU_ARCH"
  fi
fi
BINARY_SWITCHED=1
mv -f "$TMP/$BIN_NAME" "$INSTALL_DIR/$BIN_NAME"
COMMITTED=1
if [ -n "$RUNTIME_OLD" ]; then
  echo "runtime rollback: rm -rf '$RUNTIME_ROOT/$QEMU_ARCH' && mv '$RUNTIME_OLD' '$RUNTIME_ROOT/$QEMU_ARCH'"
fi
echo "installed: $INSTALL_DIR/$BIN_NAME"
case ":$PATH:" in
  *:"$INSTALL_DIR":*) ;;
  *) echo "add to PATH: export PATH=\"$INSTALL_DIR:\$PATH\"" ;;
esac
echo "next: pvm create --image ubuntu:26.04 --name dev1 --user dev  (首次会自动下载镜像并烘焙 golden，属一次性，约十几分钟)"

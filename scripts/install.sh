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
case "$S/$M" in
  Darwin/arm64) ASSET="pvm_darwin_arm64" ;;
  Linux/x86_64) ASSET="pvm_linux_amd64" ;;
  Linux/aarch64|Linux/arm64)
    echo "linux/arm64 host is not supported; for a VPS exit-agent download pvm_linux_arm64.tar.gz from releases manually" >&2; exit 1 ;;
  *) echo "unsupported host: $S/$M (supported: darwin/arm64, linux/amd64)" >&2; exit 1 ;;
esac

INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$INSTALL_DIR"
INSTALL_DIR=$(cd "$INSTALL_DIR" && pwd)
# Staging on the destination filesystem makes the final rename atomic.
LOCK_DIR="$INSTALL_DIR/.pvm-install.lock"
mkdir "$LOCK_DIR" 2>/dev/null || { echo "another installation is active (or interrupted; inspect $LOCK_DIR)" >&2; exit 1; }
TMP=$(mktemp -d "$INSTALL_DIR/.pvm-install.XXXXXX") || { rmdir "$LOCK_DIR"; exit 1; }
RUNTIME_ROOT="${PVM_HOME:-$HOME/.pvm}/runtime/qemu"
RUNTIME_STAGE=""
RUNTIME_OLD=""
RUNTIME_SWITCHED=0
COMMITTED=0
cleanup() {
  rc=$?
  trap - EXIT INT TERM
  if [ "$COMMITTED" = 0 ] && [ "$RUNTIME_SWITCHED" = 1 ]; then
    rm -f "$RUNTIME_ROOT/aarch64"
    if [ -n "$RUNTIME_OLD" ]; then
      mv "$RUNTIME_OLD" "$RUNTIME_ROOT/aarch64" || echo "runtime restore failed; restore $RUNTIME_OLD to $RUNTIME_ROOT/aarch64" >&2
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
[ "$(tar -tzf asset.tar.gz)" = "pvm" ] || { echo "unexpected binary archive entries" >&2; exit 1; }
tar -tvzf asset.tar.gz | awk 'substr($0,1,1) != "-" {bad=1} END {exit bad}' || { echo "binary must be a regular file" >&2; exit 1; }
tar -xzf asset.tar.gz pvm
[ -f pvm ] && [ ! -L pvm ] || { echo "invalid executable archive entry" >&2; exit 1; }
command -v file >/dev/null 2>&1 || { echo "file utility is required to validate the release platform" >&2; exit 1; }
BINARY_FORMAT=$(file -b pvm)
case "$S/$BINARY_FORMAT" in
  Darwin/*Mach-O*arm64*|Linux/*ELF*64-bit*x86-64*) ;;
  *) echo "release platform mismatch" >&2; exit 1 ;;
esac
chmod 755 pvm
[ "$(./pvm version)" = "$TAG" ] || { echo "release version mismatch" >&2; exit 1; }

if [ "$S" = "Darwin" ] && [ "${PVM_SKIP_QEMU:-0}" != "1" ]; then
  echo "staging bundled qemu runtime (aarch64)..."
  curl -fsSL "$BASE/$TAG/pvm-qemu-runtime-aarch64.tar.gz" -o qemu.tar.gz
  attest qemu.tar.gz
  verify pvm-qemu-runtime-aarch64.tar.gz qemu.tar.gz
  tar -tzf qemu.tar.gz > qemu.entries
  awk '/^\// || /(^|\/)\.\.(\/|$)/ || !/^aarch64(\/|$)/ {bad=1} END {exit bad}' qemu.entries || { echo "unsafe runtime archive paths" >&2; exit 1; }
  mkdir -p "$RUNTIME_ROOT"
  RUNTIME_ROOT=$(cd "$RUNTIME_ROOT" && pwd)
  RUNTIME_STAGE=$(mktemp -d "$RUNTIME_ROOT/release-${TAG}.XXXXXX")
  tar -tvzf qemu.tar.gz | awk 'substr($0,1,1) != "-" && substr($0,1,1) != "d" {bad=1} END {exit bad}' || { echo "runtime archive must contain only files and directories" >&2; exit 1; }
  tar -xzf qemu.tar.gz -C "$RUNTIME_STAGE"
  [ -x "$RUNTIME_STAGE/aarch64/bin/qemu-system-aarch64" ] || { echo "runtime executable missing" >&2; exit 1; }
  "$RUNTIME_STAGE/aarch64/bin/qemu-system-aarch64" --version >/dev/null
  "$RUNTIME_STAGE/aarch64/bin/qemu-img" --version >/dev/null
  [ -s "$RUNTIME_STAGE/aarch64/share/qemu/edk2-aarch64-code.fd" ] || { echo "runtime firmware missing" >&2; exit 1; }
  codesign --verify --strict "$RUNTIME_STAGE/aarch64/bin/qemu-system-aarch64"
  codesign -d --entitlements :- "$RUNTIME_STAGE/aarch64/bin/qemu-system-aarch64" 2>/dev/null | grep -q '<key>com.apple.security.hypervisor</key><true' || { echo "runtime HVF entitlement missing" >&2; exit 1; }
fi

# Preserve the previous binary before changing either installed component.
if [ -e "$INSTALL_DIR/pvm" ]; then
  BACKUP=$(mktemp "$INSTALL_DIR/pvm.previous.XXXXXX")
  cp -p "$INSTALL_DIR/pvm" "$BACKUP"
  echo "binary rollback: cp '$BACKUP' '$INSTALL_DIR/.pvm-rollback' && mv -f '$INSTALL_DIR/.pvm-rollback' '$INSTALL_DIR/pvm'"
fi
if [ -n "$RUNTIME_STAGE" ]; then
  if [ -e "$RUNTIME_ROOT/aarch64" ] || [ -L "$RUNTIME_ROOT/aarch64" ]; then
    RUNTIME_OLD="$RUNTIME_STAGE/previous"
    if [ -L "$RUNTIME_ROOT/aarch64" ]; then
      cp -P "$RUNTIME_ROOT/aarch64" "$RUNTIME_OLD"
    else
      mv "$RUNTIME_ROOT/aarch64" "$RUNTIME_OLD"
    fi
  fi
  RUNTIME_SWITCHED=1
  ln -s "$RUNTIME_STAGE/aarch64" "$RUNTIME_STAGE/current"
  mv -fh "$RUNTIME_STAGE/current" "$RUNTIME_ROOT/aarch64"
fi
mv -f "$TMP/pvm" "$INSTALL_DIR/pvm"
COMMITTED=1
if [ -n "$RUNTIME_OLD" ]; then
  echo "runtime rollback: rm '$RUNTIME_ROOT/aarch64' && mv '$RUNTIME_OLD' '$RUNTIME_ROOT/aarch64'"
fi
echo "installed: $INSTALL_DIR/pvm"
case ":$PATH:" in
  *:"$INSTALL_DIR":*) ;;
  *) echo "add to PATH: export PATH=\"$INSTALL_DIR:\$PATH\"" ;;
esac
echo "next: pvm create --image ubuntu:26.04 --name dev1 --user dev  (首次会自动下载镜像并烘焙 golden，属一次性，约十几分钟)"

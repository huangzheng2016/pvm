#!/usr/bin/env bash
# fetch-xpra-client.sh: download the Xpra Windows client that the installers
# ship for `pvm app`, check it, and unpack it into <dest>.
#
#   ./script/windows/fetch-xpra-client.sh <dest>
#
# The release is pinned here and must match the guest's release line
# (internal/image/xpra.go). The zip has to match the pinned SHA256 and carry a
# valid signature from xpra.org's key, whose fingerprint is pinned too.
set -euo pipefail

XPRA_ZIP=Xpra-Light-x86_64_6.5.5-r0.zip
XPRA_SHA256=878adf94ade4f04c888a9053edd940b0ff012ecfa5f23ce73a47a79c79ac45fe
XPRA_KEY_FPR=B4993B57323148E37977E5D873254CAD17978FAF
XPRA_BASE_URL=${XPRA_BASE_URL:-https://xpra.org/dists/windows}

[ $# -eq 1 ] || { echo "usage: $0 <dest>" >&2; exit 2; }
DEST=$1
[ ! -e "$DEST" ] || [ -z "$(ls -A "$DEST")" ] || { echo "error: $DEST is not empty" >&2; exit 1; }
for tool in curl gpg unzip sha256sum; do
  command -v "$tool" >/dev/null || { echo "error: $tool is required" >&2; exit 1; }
done

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
fetch() { curl -fsSL --retry 3 --connect-timeout 20 --max-time 900 -o "$WORK/$2" "$1"; }
fetch "$XPRA_BASE_URL/$XPRA_ZIP" "$XPRA_ZIP"
fetch "$XPRA_BASE_URL/$XPRA_ZIP.gpg" "$XPRA_ZIP.gpg"
fetch https://xpra.org/xpra.asc xpra.asc

got=$(sha256sum "$WORK/$XPRA_ZIP" | awk '{print $1}')
[ "$got" = "$XPRA_SHA256" ] || { echo "error: $XPRA_ZIP has SHA256 $got, want $XPRA_SHA256" >&2; exit 1; }

# relative paths: gpg under Git Bash on Windows misreads absolute ones
mkdir "$WORK/gnupg"
chmod 700 "$WORK/gnupg"
(
  cd "$WORK"
  gpg --homedir gnupg --batch --quiet --import xpra.asc 2>/dev/null
  # VALIDSIG carries the fingerprint of the key that made the signature
  gpg --homedir gnupg --batch --status-fd 1 --verify "$XPRA_ZIP.gpg" "$XPRA_ZIP" 2>/dev/null \
    | grep -q "^\[GNUPG:\] VALIDSIG $XPRA_KEY_FPR "
) || { echo "error: $XPRA_ZIP is not signed by $XPRA_KEY_FPR" >&2; exit 1; }

mkdir -p "$DEST"
unzip -q "$WORK/$XPRA_ZIP" -d "$WORK/unpacked"
src=$(find "$WORK/unpacked" -mindepth 1 -maxdepth 1 -type d)
[ -f "$src/Xpra.exe" ] && [ -f "$src/Xpra_cmd.exe" ] && [ -f "$src/share/xpra/COPYING" ] \
  || { echo "error: unexpected layout in $XPRA_ZIP" >&2; exit 1; }
cp -R "$src"/. "$DEST"/
cat > "$DEST/XPRA-SOURCE.txt" <<EOF
This folder holds the Xpra client ${XPRA_ZIP%.zip}, unmodified, as published
at $XPRA_BASE_URL/$XPRA_ZIP. Xpra is free software under the GNU General
Public License version 2 or later; see share/xpra/COPYING. Its source code
is available at https://github.com/Xpra-org/xpra and https://xpra.org/src/.
EOF
echo "unpacked $XPRA_ZIP into $DEST"

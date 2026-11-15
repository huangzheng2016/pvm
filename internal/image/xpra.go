package image

// xpraKeyFingerprint pins the signing key of xpra.org, which signs both
// its APT repository and its Windows client packages.
const xpraKeyFingerprint = "B4993B57323148E37977E5D873254CAD17978FAF"

// XpraVersion is the release line pvm uses for `pvm app`, in the guest and
// on the host; client and server must speak the same protocol.
const XpraVersion = "6.5"

// XpraPackages are the server side of `pvm app`: the X11 server, codecs
// and Xvfb, and xclip, which sets the guest clipboard to host content.
// Ubuntu and Debian do not package a current Xpra.
var XpraPackages = []string{"xpra-server", "xpra-x11", "xpra-codecs", "xclip"}

// XpraMaskScript masks the packaged system-wide Xpra service, whose socket
// listens on every interface; pvm runs its own server per user.
const XpraMaskScript = `systemctl mask --now xpra-server.socket xpra-server.service >/dev/null 2>&1 || true
`

// XpraRepoScript adds xpra.org's APT repository for the running release,
// pinned to XpraVersion. It runs as root under bash and fails unless the
// downloaded key has exactly one primary key with the pinned fingerprint.
const XpraRepoScript = `. /etc/os-release
xprakey=$(mktemp)
curl -fsSL --retry 3 --connect-timeout 20 --max-time 120 https://xpra.org/xpra.asc -o "$xprakey"
xprafpr=$(GNUPGHOME=$(mktemp -d) gpg --show-keys --with-colons "$xprakey" | awk -F: '$1=="pub"{n++} $1=="fpr"&&f==""{f=$10} END{if(n==1)print f}')
if [ "$xprafpr" != "` + xpraKeyFingerprint + `" ]; then
  echo "xpra signing key fingerprint mismatch: got '$xprafpr'" >&2
  exit 1
fi
install -d -m 0755 /etc/apt/keyrings
install -m 0644 "$xprakey" /etc/apt/keyrings/xpra.asc
rm -f "$xprakey"
cat > /etc/apt/sources.list.d/xpra.sources <<EOF
Types: deb
URIs: https://xpra.org
Suites: $VERSION_CODENAME
Components: main
Signed-By: /etc/apt/keyrings/xpra.asc
EOF
# client and server must stay on the same release line
cat > /etc/apt/preferences.d/xpra <<'EOF'
Package: xpra*
Pin: version ` + XpraVersion + `.*
Pin-Priority: 990
EOF
`

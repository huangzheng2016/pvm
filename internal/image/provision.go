package image

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/huangzheng2016/pvm/internal/vm"
)

var GoldenPackages = []string{
	"docker.io", "qemu-guest-agent",
	"build-essential", "git", "curl", "jq", "python3-pip",
	"htop", "vim", "unzip", "ca-certificates", "fonts-noto-cjk",
	"sshfs", "firefox",
}

// mozillaKeyFingerprint pins the signing key of packages.mozilla.org.
// Ubuntu's own firefox package only installs the snap, and Debian ships
// firefox-esr, so firefox comes from Mozilla's repository on both.
const mozillaKeyFingerprint = "35BAA0B33E9EB396F59CA838C0BA5CE6DC6315A3"

// DesktopPackages omits a display manager; VNC sessions start per user on demand.
var DesktopPackages = []string{
	"xfwm4", "xfce4-panel", "xfdesktop4", "xfce4-session", "xfce4-settings",
	"xfconf", "thunar", "xfce4-terminal", "dbus-x11", "xterm",
	"tigervnc-standalone-server", "novnc", "websockify", "x11-xserver-utils", "ibus-libpinyin",
}

// The provision script deletes buildUser before returning, so subsequent
// shutdown must use QMP rather than SSH.
// goldenInstallArgs are the apt-get install arguments of the bake.
// lightdm/xserver-xorg come in via Recommends (xfce4-session -> light-locker
// -> lightdm -> xserver-xorg) but VMs have no GPU: lightdm fails to start a
// local Xorg and restart-loops. The TigerVNC stack needs neither; block them.
func goldenInstallArgs() string {
	return strings.Join(GoldenPackages, " ") + " " + strings.Join(DesktopPackages, " ") +
		" lightdm- light-locker- xfce4-screensaver- xserver-xorg- xserver-xorg-core-"
}

// prefetchDownloads is how many packages download at once. apt itself
// fetches from one mirror over a single connection, which leaves slow or
// distant links mostly idle.
const prefetchDownloads = 8

// prefetchScript defines pvm_prefetch, which puts the debs that the upgrade
// and the install need into apt's cache before apt runs. Each file is
// checked against the hash from the signed index before it enters the
// cache. A file that fails to download is left to apt, which fetches it
// the normal way, so a prefetch failure only costs time.
var prefetchScript = `pvm_fetch_one() {
  local dest=/var/cache/apt/archives sum tmp
  case "$3" in
    SHA512) sum=sha512sum ;;
    SHA256) sum=sha256sum ;;
    *) echo "$2" >> "$pvm_prefetch_failed"; return 0 ;;
  esac
  [ -e "$dest/$2" ] && return 0
  tmp="$dest/partial/$2.pvm-prefetch"
  # a stalled transfer must fail over to apt rather than block xargs
  if curl -fsSL --retry 3 --connect-timeout 20 --speed-limit 1024 --speed-time 60 -o "$tmp" "$1" && echo "$4  $tmp" | "$sum" -c --status; then
    mv -f "$tmp" "$dest/$2"
  else
    rm -f "$tmp"
    echo "$2" >> "$pvm_prefetch_failed"
  fi
}
pvm_prefetch() {
  local list
  list=$(mktemp)
  pvm_prefetch_failed=$(mktemp)
  export pvm_prefetch_failed
  export -f pvm_fetch_one
  # --print-uris lines: 'URL' FILE SIZE HASHTYPE:HASH. A resolver error
  # here only skips the prefetch; the real apt-get run reports it.
  if ! { apt-get -qq -y --print-uris upgrade && apt-get -qq -y --print-uris install ` + goldenInstallArgs() + `; } > "$list.uris"; then
    echo "[provision] could not list package downloads; apt downloads them itself" >&2
  fi
  sed -n "s/^'\([^']*\)' \([^ ]*\) \([0-9]*\) \([A-Za-z0-9]*\):\([0-9a-fA-F]*\)$/\1 \2 \4 \5 \3/p" "$list.uris" | sort -u -k2,2 > "$list"
  echo "[provision] $(wc -l < "$list") packages, $(awk '{s+=$5} END {printf "%.0f", s/1048576}' "$list") MiB"
  cut -d' ' -f1-4 "$list" | xargs -r -n 4 -P ` + strconv.Itoa(prefetchDownloads) + ` bash -c 'pvm_fetch_one "$@"' _
  if [ -s "$pvm_prefetch_failed" ]; then
    echo "[provision] $(wc -l < "$pvm_prefetch_failed") packages were not prefetched; apt downloads them itself:" >&2
    sed 's/^/  /' "$pvm_prefetch_failed" >&2
  fi
  rm -f "$list" "$list.uris" "$pvm_prefetch_failed"
  unset pvm_prefetch_failed
}`

func renderProvision(distro, buildUser string, proxy ProxyConfig) (string, error) {
	switch distro {
	case "ubuntu", "debian":
	default:
		return "", fmt.Errorf("no provisioner for distro %q", distro)
	}
	if buildUser == "" || buildUser == "root" {
		return "", fmt.Errorf("invalid build user %q", buildUser)
	}
	if err := vm.ValidateSpec(&vm.Spec{User: buildUser}, nil); err != nil {
		return "", err
	}
	if err := proxy.validate(); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(`#!/bin/bash
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

# wait for the base image's own cloud-init/apt to settle
if command -v cloud-init >/dev/null; then cloud-init status --wait; fi
for i in $(seq 1 300); do
  fuser /var/lib/dpkg/lock-frontend >/dev/null 2>&1 || break
  sleep 2
done
if fuser /var/lib/dpkg/lock-frontend >/dev/null 2>&1; then
  echo "dpkg lock still held after 10min:" >&2
  fuser -v /var/lib/dpkg/lock-frontend >&2 || true
  exit 1
fi

` + func() string {
		var b strings.Builder
		proxy.writeProvisionEnv(&b)
		return b.String()
	}() + `
echo "[provision] adding the Mozilla APT repository..."
apt-get update
apt-get -y install ca-certificates curl gpg
mozkey=$(mktemp)
curl -fsSL --retry 3 --connect-timeout 20 --max-time 120 https://packages.mozilla.org/apt/repo-signing-key.gpg -o "$mozkey"
# exactly one primary key, and its fingerprint must match; a second key in
# the file would otherwise be trusted for the repository as well
mozfpr=$(GNUPGHOME=$(mktemp -d) gpg --show-keys --with-colons "$mozkey" | awk -F: '$1=="pub"{n++} $1=="fpr"&&f==""{f=$10} END{if(n==1)print f}')
if [ "$mozfpr" != "` + mozillaKeyFingerprint + `" ]; then
  echo "Mozilla signing key fingerprint mismatch: got '$mozfpr'" >&2
  exit 1
fi
install -d -m 0755 /etc/apt/keyrings
install -m 0644 "$mozkey" /etc/apt/keyrings/packages.mozilla.org.asc
rm -f "$mozkey"
cat > /etc/apt/sources.list.d/mozilla.sources <<'EOF'
Types: deb
URIs: https://packages.mozilla.org/apt
Suites: mozilla
Components: main
Signed-By: /etc/apt/keyrings/packages.mozilla.org.asc
EOF
# priority 1000 lets Mozilla's firefox beat Ubuntu's higher-epoch snap
# transitional package
cat > /etc/apt/preferences.d/mozilla <<'EOF'
Package: *
Pin: origin packages.mozilla.org
Pin-Priority: 1000
EOF

echo "[provision] adding the xpra APT repository..."
` + XpraRepoScript + `
echo "[provision] updating package lists..."
apt-get update
# e.g. debian forky has no tigervnc: fail before the long upgrade
apt-cache show tigervnc-standalone-server novnc websockify >/dev/null 2>&1 || { echo "desktop packages (tigervnc/novnc/websockify) unavailable on this release" >&2; exit 1; }
apt-cache show ` + strings.Join(XpraPackages, " ") + ` >/dev/null 2>&1 || { echo "xpra packages unavailable for this release from xpra.org" >&2; exit 1; }
` + prefetchScript + `
echo "[provision] downloading packages in parallel..."
pvm_prefetch
echo "[provision] upgrading system packages (this may take 5-10 minutes)..."
apt-get -y upgrade
echo "[provision] installing docker, build tools, and desktop packages (this may take 10-15 minutes)..."
apt-get -y install ` + goldenInstallArgs() + `
# without recommends: they pull in CUPS and a web client
apt-get -y install --no-install-recommends ` + strings.Join(XpraPackages, " ") + `
` + XpraMaskScript + `
echo "[provision] configuring desktop environment..."
cat > /etc/environment.d/ibus.conf <<'EOF'
GTK_IM_MODULE=ibus
QT_IM_MODULE=ibus
XMODIFIERS=@im=ibus
EOF

mkdir -p /etc/skel/.config/autostart
cat > /etc/skel/.config/autostart/ibus.desktop <<'EOF'
[Desktop Entry]
Type=Application
Name=IBus
Exec=ibus-daemon -drx
X-GNOME-Autostart-enabled=true
EOF

cat > /etc/skel/.config/autostart/xfce4-screensaver.desktop <<'EOF'
[Desktop Entry]
Type=Application
Name=Screensaver
Exec=xfce4-screensaver
Hidden=true
EOF

mkdir -p /etc/skel/.config/xfce4/xfconf/xfce-perchannel-xml
cat > /etc/skel/.config/xfce4/xfconf/xfce-perchannel-xml/xfce4-screensaver.xml <<'EOF'
<?xml version="1.1" encoding="UTF-8"?>
<channel name="xfce4-screensaver" version="1.0">
  <property name="general" type="empty">
    <property name="enabled" type="bool" value="false"/>
  </property>
</channel>
EOF

echo "[provision] starting docker service..."
systemctl enable docker
systemctl start docker
# wait for the daemon to come up
for i in $(seq 1 30); do docker info >/dev/null 2>&1 && break; sleep 2; done
docker info

# colord prompts for polkit auth ("create a color managed device") on every
# X session start; pvm users have no password, so the dialog is unanswerable.
cat > /etc/polkit-1/rules.d/49-pvm-colord.rules <<'EOF'
polkit.addRule(function(action, subject) {
    if (action.id.indexOf("org.freedesktop.color-manager.") === 0) {
        return polkit.Result.YES;
    }
});
EOF

echo "[provision] cleaning up and preparing image for cloning..."
# scrub machine-specific state so clones re-run cloud-init cleanly
apt-get clean
rm -rf /var/lib/apt/lists/*
docker container prune -f >/dev/null 2>&1 || true
docker builder prune -af >/dev/null 2>&1 || true
docker image prune -f >/dev/null 2>&1 || true
journalctl --vacuum-time=1s || true
rm -rf /var/log/*.gz /var/log/*.[0-9] || true
if command -v cloud-init >/dev/null; then cloud-init clean --logs --seed; fi
rm -rf /var/lib/cloud/instances/* || true
# per-clone uniqueness: host keys, machine-id, fstab share entry
rm -f /etc/ssh/ssh_host_*
truncate -s 0 /etc/machine-id
rm -f /var/lib/dbus/machine-id
sed -i '/^pvmshare /d' /etc/fstab
sync

echo "[provision] finalizing golden image (removing build user)..."
# last: drop the build user (it holds the host uid which clones reuse).
# After this no further ssh is possible; shutdown happens via QMP.
` + func() string {
		var b strings.Builder
		proxy.writeCleanup(&b)
		return b.String()
	}() + `userdel -rf ` + buildUser + `
getent passwd ` + buildUser + ` >/dev/null && { echo "build user ` + buildUser + ` still exists; refusing to seal the golden image" >&2; exit 1; } || true
echo "[provision] golden image provisioning complete"
fstrim -av || true
sync
`)
	return b.String(), nil
}

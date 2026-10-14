package image

import (
	"fmt"
	"strings"
)

var GoldenPackages = []string{
	"docker.io", "qemu-guest-agent",
	"build-essential", "git", "curl", "jq", "python3-pip",
	"htop", "vim", "unzip", "ca-certificates", "fonts-noto-cjk",
}

// DesktopPackages omits a display manager; VNC sessions start per user on demand.
var DesktopPackages = []string{
	"xfwm4", "xfce4-panel", "xfdesktop4", "xfce4-session", "xfce4-settings",
	"xfconf", "thunar", "xfce4-terminal", "dbus-x11", "xterm",
	"tigervnc-standalone-server", "novnc", "websockify", "x11-xserver-utils", "ibus-libpinyin",
}

// The provision script deletes buildUser before returning, so subsequent
// shutdown must use QMP rather than SSH.
func renderProvision(distro, buildUser string, proxy ProxyConfig) (string, error) {
	switch distro {
	case "ubuntu", "debian":
	default:
		return "", fmt.Errorf("no provisioner for distro %q", distro)
	}
	if buildUser == "" || buildUser == "root" {
		return "", fmt.Errorf("invalid build user %q", buildUser)
	}
	var b strings.Builder
	b.WriteString(`#!/bin/bash
set -euxo pipefail
export DEBIAN_FRONTEND=noninteractive

# wait for the base image's own cloud-init/apt to settle
if command -v cloud-init >/dev/null; then cloud-init status --wait >/dev/null; fi
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
apt-get update
# e.g. debian forky has no tigervnc: fail before the long upgrade
apt-cache show tigervnc-standalone-server novnc websockify >/dev/null 2>&1 || { echo "desktop packages (tigervnc/novnc/websockify) unavailable on this release" >&2; exit 1; }
apt-get -y upgrade
# lightdm/xserver-xorg come in via Recommends (xfce4-session -> light-locker
# -> lightdm -> xserver-xorg) but VMs have no GPU: lightdm fails to start a
# local Xorg and restart-loops. The TigerVNC stack needs neither; block them.
apt-get -y install ` + strings.Join(GoldenPackages, " ") + ` ` + strings.Join(DesktopPackages, " ") + ` lightdm- light-locker- xserver-xorg- xserver-xorg-core-

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

mkdir -p /etc/skel/.config/xfce4/xfconf/xfce-perchannel-xml
cat > /etc/skel/.config/xfce4/xfconf/xfce-perchannel-xml/xfce4-screensaver.xml <<'EOF'
<?xml version="1.1" encoding="UTF-8"?>
<channel name="xfce4-screensaver" version="1.0">
  <property name="general" type="empty">
    <property name="enabled" type="bool" value="false"/>
  </property>
</channel>
EOF

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

# scrub machine-specific state so clones re-run cloud-init cleanly
apt-get clean
rm -rf /var/lib/apt/lists/*
docker container prune -f >/dev/null 2>&1 || true
docker builder prune -af >/dev/null 2>&1 || true
docker image prune -f >/dev/null 2>&1 || true
journalctl --vacuum-time=1s || true
rm -rf /var/log/*.gz /var/log/*.[0-9] || true
# early: hole punching keeps the host in disk io for minutes; drain before shutdown
fstrim -av || true
if command -v cloud-init >/dev/null; then cloud-init clean --logs --seed; fi
rm -rf /var/lib/cloud/instances/* || true
# per-clone uniqueness: host keys, machine-id, fstab share entry
rm -f /etc/ssh/ssh_host_*
truncate -s 0 /etc/machine-id
rm -f /var/lib/dbus/machine-id
sed -i '/^pvmshare /d' /etc/fstab
sync

# last: drop the build user (it holds the host uid which clones reuse).
# After this no further ssh is possible; shutdown happens via QMP.
` + func() string {
		var b strings.Builder
		proxy.writeCleanup(&b)
		return b.String()
	}() + `userdel -rf ` + buildUser + `
getent passwd ` + buildUser + ` >/dev/null && { echo "build user ` + buildUser + ` still exists; refusing to seal the golden image" >&2; exit 1; } || true
sync
`)
	return b.String(), nil
}

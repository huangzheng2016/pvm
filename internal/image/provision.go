package image

import (
	"fmt"
	"strings"
)

// GoldenPackages are apt packages baked into every golden image.
var GoldenPackages = []string{
	"docker.io", "qemu-guest-agent",
	"build-essential", "git", "curl", "jq", "python3-pip",
	"htop", "vim", "unzip", "ca-certificates", "fonts-noto-cjk",
}

// DesktopPackages are the native XFCE + TigerVNC + noVNC stack baked into
// golden images and launched on demand by `pvm desktop`. No display
// manager: the VNC session is started per-user only when requested.
var DesktopPackages = []string{
	"xfwm4", "xfce4-panel", "xfdesktop4", "xfce4-session", "xfce4-settings",
	"xfconf", "thunar", "xfce4-terminal", "dbus-x11", "xterm",
	"tigervnc-standalone-server", "novnc", "websockify", "x11-xserver-utils",
}

// renderProvision produces the bash script executed over SSH inside the
// build VM. The script is self-terminating: it deletes the build user at
// the end, so nothing may ssh into the VM afterwards (poweroff is done
// via QMP, not ssh).
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
cloud-init status --wait >/dev/null 2>&1 || true
while fuser /var/lib/dpkg/lock-frontend >/dev/null 2>&1; do sleep 2; done

` + func() string {
		var b strings.Builder
		proxy.writeProvisionEnv(&b)
		return b.String()
	}() + `
apt-get update
apt-get -y upgrade
apt-get -y install ` + strings.Join(GoldenPackages, " ") + ` ` + strings.Join(DesktopPackages, " ") + `

systemctl enable docker
systemctl start docker
# wait for the daemon to come up
for i in $(seq 1 30); do docker info >/dev/null 2>&1 && break; sleep 2; done
docker info

# scrub machine-specific state so clones re-run cloud-init cleanly
apt-get clean
rm -rf /var/lib/apt/lists/*
docker container prune -f >/dev/null 2>&1 || true
docker builder prune -af >/dev/null 2>&1 || true
docker image prune -f >/dev/null 2>&1 || true
journalctl --vacuum-time=1s || true
rm -rf /var/log/*.gz /var/log/*.[0-9] || true
cloud-init clean --logs --seed || true
rm -rf /var/lib/cloud/instances/* || true
# per-clone uniqueness: host keys, machine-id, fstab share entry
rm -f /etc/ssh/ssh_host_*
truncate -s 0 /etc/machine-id
rm -f /var/lib/dbus/machine-id
sed -i '/^pvmshare /d' /etc/fstab
fstrim -av || true
sync

# last: drop the build user (it holds the host uid which clones reuse).
# After this no further ssh is possible; shutdown happens via QMP.
` + func() string {
		var b strings.Builder
		proxy.writeCleanup(&b)
		return b.String()
	}() + `userdel -rf ` + buildUser + ` || true
sync
`)
	return b.String(), nil
}

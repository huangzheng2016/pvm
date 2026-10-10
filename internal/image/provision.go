package image

import (
	"fmt"
	"strings"
)

// KasmImage is the kasm desktop image pre-baked into golden images and
// launched by `pvm kasm`; multi-arch (amd64+arm64 manifests verified).
const KasmImage = "kasmweb/core-ubuntu-jammy:1.16.0"

// GoldenPackages are apt packages baked into every golden image.
var GoldenPackages = []string{
	"docker.io", "qemu-guest-agent",
	"build-essential", "git", "curl", "jq", "python3-pip",
	"htop", "vim", "unzip", "ca-certificates", "fonts-noto-cjk",
}

// renderProvision produces the bash script executed over SSH inside the
// build VM. The script is self-terminating: it deletes the build user at
// the end, so nothing may ssh into the VM afterwards (poweroff is done
// via QMP, not ssh).
func renderProvision(distro, buildUser string) (string, error) {
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

apt-get update
apt-get -y upgrade
apt-get -y install ` + strings.Join(GoldenPackages, " ") + `

systemctl enable docker
systemctl start docker
# wait for the daemon to come up
for i in $(seq 1 30); do docker info >/dev/null 2>&1 && break; sleep 2; done
docker info

: > /etc/pvm-preinstalled-images
docker pull ` + KasmImage + ` && echo ` + KasmImage + ` >> /etc/pvm-preinstalled-images

echo PVM_PREINSTALLED_BEGIN
cat /etc/pvm-preinstalled-images
echo PVM_PREINSTALLED_END

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
userdel -rf ` + buildUser + ` || true
sync
`)
	return b.String(), nil
}

// parsePreinstalled extracts the docker image list captured between the
// provision script's markers.
func parsePreinstalled(provisionLog string) []string {
	start := strings.Index(provisionLog, "PVM_PREINSTALLED_BEGIN\n")
	end := strings.Index(provisionLog, "PVM_PREINSTALLED_END")
	if start < 0 || end < 0 || end <= start {
		return nil
	}
	body := provisionLog[start+len("PVM_PREINSTALLED_BEGIN\n") : end]
	var out []string
	for line := range strings.Lines(body) {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "/") && !strings.HasPrefix(line, "+") {
			out = append(out, line)
		}
	}
	return out
}

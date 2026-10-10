package vm

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// GuestMount is one host->guest 9p mount beyond the default /share.
type GuestMount struct {
	Tag   string
	Host  string
	Guest string
}

func userData(spec *Spec, sshPub string, mounts []GuestMount) ([]byte, error) {
	fstab := []string{
		"pvmshare /share 9p trans=virtio,version=9p2000.L,msize=262144,access=any,rw,_netdev,nofail 0 0",
	}
	runcmd := []string{
		"mkdir -p /share",
		"mountpoint -q /share || mount -t 9p -o trans=virtio,version=9p2000.L,msize=262144,access=any pvmshare /share",
	}
	for _, m := range mounts {
		fstab = append(fstab, fmt.Sprintf("%s %s 9p trans=virtio,version=9p2000.L,msize=262144,access=any,rw,_netdev,nofail 0 0", m.Tag, m.Guest))
		runcmd = append(runcmd,
			"mkdir -p "+m.Guest,
			fmt.Sprintf("mountpoint -q %s || mount -t 9p -o trans=virtio,version=9p2000.L,msize=262144,access=any %s %s", m.Guest, m.Tag, m.Guest),
		)
	}

	// Git identity is written only when explicitly requested at create
	// time; the host's ~/.gitconfig is never consulted or propagated
	// (it leaks into Claude Code's gitStatus session context).
	if spec.GitEmail != "" {
		git := []string{
			fmt.Sprintf("sudo -u %s git config --global user.email %q", spec.User, spec.GitEmail),
		}
		if spec.GitName != "" {
			git = append(git, fmt.Sprintf("sudo -u %s git config --global user.name %q", spec.User, spec.GitName))
		}
		runcmd = append(git, runcmd...)
	}

	doc := map[string]any{
		"hostname":   spec.Name,
		"fqdn":       spec.Name + ".pvm.internal",
		"ssh_pwauth": false,
		"runcmd":     runcmd,
		// Neutral default: a plain US-English Ubuntu, not the C.UTF-8
		// cloud-image fallback.
		"locale": "en_US.UTF-8",
	}
	if spec.Locale != "" {
		doc["locale"] = spec.Locale
	}
	if spec.TZ != "" {
		doc["timezone"] = spec.TZ
	}
	if spec.Golden > 0 {
		// Golden images are already provisioned (docker, qga, dev tools):
		// skip the first-boot apt storm and just attach the user to docker.
		// Do not --now qemu-guest-agent: its unit waits on the qga virtio
		// port which pvm does not attach, stalling boot for 90s.
		doc["package_update"] = false
		doc["runcmd"] = append(runcmd,
			"systemctl enable --now docker",
		)
	} else {
		doc["package_update"] = true
		doc["packages"] = []string{"qemu-guest-agent"}
	}
	groups := []string{"sudo"}
	if spec.Golden > 0 {
		groups = append(groups, "docker")
	}
	doc["users"] = []map[string]any{{
		"name":                spec.User,
		"groups":              groups,
		"shell":               "/bin/bash",
		"sudo":                "ALL=(ALL) NOPASSWD:ALL",
		"lock_passwd":         true,
		"uid":                 spec.GuestUID,
		"ssh_authorized_keys": []string{strings.TrimSpace(sshPub)},
	}}
	doc["write_files"] = []map[string]any{{
		"path":    "/etc/fstab",
		"append":  true,
		"content": strings.Join(fstab, "\n") + "\n",
	}}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return append([]byte("#cloud-config\n"), out...), nil
}

func metaData(spec *Spec) []byte {
	return []byte(fmt.Sprintf("instance-id: pvm-%s\nlocal-hostname: %s\n", spec.Name, spec.Name))
}

// gatewayUserData provisions a gateway VM: pvmgw user, nftables, the
// pvm-gateway systemd unit (enabled once the host pushes the binary)
// and ip_forward.
func gatewayUserData(spec *Spec, sshPub string) ([]byte, error) {
	unit := `[Unit]
Description=pvm gateway agent
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/pvm-gateway -config /etc/pvm/gateway.json
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
`
	doc := map[string]any{
		"hostname":       spec.Name,
		"ssh_pwauth":     false,
		"package_update": true,
		"packages":       []string{"nftables", "iproute2"},
		"runcmd": []string{
			"mkdir -p /etc/pvm",
			"printf 'net.ipv4.ip_forward=1\\n' > /etc/sysctl.d/99-pvm.conf",
			"sysctl --system",
			"systemctl daemon-reload",
			"systemctl enable pvm-gateway.service",
		},
		"users": []map[string]any{{
			"name":                spec.User,
			"groups":              []string{"sudo"},
			"shell":               "/bin/bash",
			"sudo":                "ALL=(ALL) NOPASSWD:ALL",
			"lock_passwd":         true,
			"uid":                 spec.GuestUID,
			"ssh_authorized_keys": []string{strings.TrimSpace(sshPub)},
		}},
		"write_files": []map[string]any{{
			"path":        "/etc/systemd/system/pvm-gateway.service",
			"content":     unit,
			"permissions": "0644",
		}},
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return append([]byte("#cloud-config\n"), out...), nil
}

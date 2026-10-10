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

	doc := map[string]any{
		"hostname":       spec.Name,
		"fqdn":           spec.Name + ".pvm.internal",
		"ssh_pwauth":     false,
		"package_update": true,
		"packages":       []string{"qemu-guest-agent"},
		"runcmd":         runcmd,
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
			"path":    "/etc/fstab",
			"append":  true,
			"content": strings.Join(fstab, "\n") + "\n",
		}},
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return append([]byte("#cloud-config\n"), out...), nil
}

func metaData(spec *Spec) []byte {
	return []byte(fmt.Sprintf("instance-id: pvm-%s\nlocal-hostname: %s\n", spec.Name, spec.Name))
}

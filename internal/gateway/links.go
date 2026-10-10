package gateway

import (
	"fmt"
	"log"
	"os/exec"
	"sort"
	"strings"
)

// applyNetwork creates br0, assigns the gateway address, enables
// forwarding and renders the nftables ruleset from the config.
func (a *Agent) applyNetwork(c *Config) error {
	upl, err := uplinkIface()
	if err != nil {
		return fmt.Errorf("uplink: %w", err)
	}
	a.mu.Lock()
	a.upl = upl
	a.mu.Unlock()

	if err := run("ip", "link", "add", "name", a.br, "type", "bridge"); err != nil && !strings.Contains(err.Error(), "File exists") {
		return fmt.Errorf("create bridge: %w", err)
	}
	// idempotent-ish: adding the same addr again errors, ignore that one
	if out, err := exec.Command("ip", "addr", "replace", c.GwIP+"/"+maskBits(c.CIDR), "dev", a.br).CombinedOutput(); err != nil {
		return fmt.Errorf("bridge addr: %v\n%s", err, out)
	}
	if err := run("ip", "link", "set", a.br, "up"); err != nil {
		return err
	}
	if err := os_WriteSysctl(); err != nil {
		return err
	}
	return ApplyNFT(RenderNFT(c, upl, a.br))
}

func os_WriteSysctl() error {
	return run("sysctl", "-w", "net.ipv4.ip_forward=1")
}

// uplinkIface returns the interface owning the default route.
func uplinkIface() (string, error) {
	out, err := exec.Command("ip", "route", "show", "default").Output()
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			return fields[i+1], nil
		}
	}
	return "", fmt.Errorf("no default route in %q", out)
}

// reconcileLinks enslaves hot-plugged NICs into br0. Any non-uplink,
// non-bridge physical interface is assumed to be a VM link.
func (a *Agent) reconcileLinks() {
	a.mu.Lock()
	upl, br := a.upl, a.br
	a.mu.Unlock()
	if upl == "" {
		return
	}
	entries, err := listIfaces()
	if err != nil {
		return
	}
	known := map[string]bool{"lo": true, upl: true, br: true}
	sort.Strings(entries)
	for _, ifname := range entries {
		if known[ifname] || strings.HasPrefix(ifname, "veth") || strings.HasPrefix(ifname, "docker") || strings.HasPrefix(ifname, "pvmwg") {
			continue
		}
		master, _ := ifaceMaster(ifname)
		if master == br {
			continue
		}
		log.Printf("enslave %s into %s", ifname, br)
		if err := run("ip", "link", "set", ifname, "master", br); err != nil {
			log.Printf("enslave %s: %v", ifname, err)
			continue
		}
		if err := run("ip", "link", "set", ifname, "up"); err != nil {
			log.Printf("link up %s: %v", ifname, err)
		}
	}
}

func listIfaces() ([]string, error) {
	out, err := exec.Command("ls", "/sys/class/net").Output()
	if err != nil {
		return nil, err
	}
	var ifs []string
	for _, f := range strings.Fields(string(out)) {
		ifs = append(ifs, f)
	}
	return ifs, nil
}

func ifaceMaster(ifname string) (string, error) {
	out, err := exec.Command("readlink", "/sys/class/net/"+ifname+"/master").Output()
	if err != nil {
		return "", err
	}
	// readlink yields the full /sys/devices/... path; keep the basename
	parts := strings.Split(strings.TrimSpace(string(out)), "/")
	return parts[len(parts)-1], nil
}

func maskBits(cidr string) string {
	_, after, ok := strings.Cut(cidr, "/")
	if !ok {
		return "24"
	}
	return after
}

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return nil
}

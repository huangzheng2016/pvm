package gateway

import (
	"fmt"
	"log"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// fake-ip UDP policy routing: the fakeudp chain marks matching flows and
// tproxies them to the gateway listener; the marked lookup delivers them
// locally so the original destination survives.
const (
	fakeUDPMark       = 0x50
	fakeUDPRouteTable = 80
	fakeUDPRulePref   = 20
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
	if out, err := exec.Command("ip", "addr", "replace", c.GwIP+"/"+maskBits(c.CIDR), "dev", a.br).CombinedOutput(); err != nil {
		return fmt.Errorf("bridge addr: %v\n%s", err, out)
	}
	if err := run("ip", "link", "set", a.br, "up"); err != nil {
		return err
	}
	if err := ensureFakeUDPRoute(); err != nil {
		return err
	}
	if err := ApplyNFT(RenderNFT(c, upl, a.br, a.exits.BypassList())); err != nil {
		_ = run("sysctl", "-w", "net.ipv4.ip_forward=0")
		return err
	}
	return os_WriteSysctl()
}

var nftListTable = func() (string, error) {
	out, err := exec.Command("nft", "list", "table", "inet", "pvm").CombinedOutput()
	return string(out), err
}

func (a *Agent) checkNFT(c *Config) error {
	out, err := nftListTable()
	if err != nil {
		return fmt.Errorf("list pvm table: %w", err)
	}
	upl := a.upl
	if upl == "" {
		upl, err = uplinkIface()
		if err != nil {
			return err
		}
	}
	// nft pads marks to 8 hex digits and flattens single-element oifname
	// sets; each required rule lists the accepted spellings.
	tproxy := fmt.Sprintf(`iifname "br0" ip daddr 198.18.0.0/16 udp dport != 53 tproxy ip to %s:%d`, c.GwIP, fakeUDPPort)
	required := [][]string{
		{
			tproxy + fmt.Sprintf(" meta mark set 0x%x", fakeUDPMark),
			tproxy + fmt.Sprintf(" meta mark set 0x%08x", fakeUDPMark),
		},
		{`iifname "br0" ip daddr 198.18.0.0/16 tcp dport != 53 redirect to :7893`},
		{`iifname "br0" ip daddr 198.18.0.0/16 icmp type echo-request dnat ip to ` + c.GwIP},
		{`iifname "br0" oifname "` + upl + `" drop`},
	}
	if len(c.Exits) > 0 {
		alts := []string{fmt.Sprintf(`oifname { %s } masquerade`, quotedTunnels(c.Exits))}
		if len(c.Exits) == 1 {
			alts = append(alts, fmt.Sprintf(`oifname %q masquerade`, tunnelIfName(0)))
		}
		required = append(required, alts)
	}
	for _, alts := range required {
		found := false
		for _, want := range alts {
			if strings.Contains(out, want) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("missing rule %q", alts[0])
		}
	}
	// Repairing the rule/route here keeps their drift from forcing a full
	// tunnel rebuild.
	if err := ensureFakeUDPRoute(); err != nil {
		return fmt.Errorf("repair fake-udp route: %w", err)
	}
	return nil
}

// ensureFakeUDPRoute installs the policy rule and local route that deliver
// fake-ip UDP flows (marked by the fakeudp chain) to the tproxy listener
// on the gateway. `ip rule` already tolerates "File exists"; the route
// replace is idempotent.
func ensureFakeUDPRoute() error {
	if err := ipRuleCmd("add", "fwmark", strconv.Itoa(fakeUDPMark), "lookup", strconv.Itoa(fakeUDPRouteTable), "pref", strconv.Itoa(fakeUDPRulePref)); err != nil {
		return fmt.Errorf("fake-udp fwmark rule: %w", err)
	}
	if err := ipRoute("replace", "local", "default", "dev", "lo", "table", strconv.Itoa(fakeUDPRouteTable)); err != nil {
		return fmt.Errorf("fake-udp local route: %w", err)
	}
	return nil
}

func os_WriteSysctl() error {
	if err := run("sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
		return err
	}
	// the cluster is IPv4-only; v6 egress has no kill-switch coverage and
	// must not exist. cloud-init writes the same values persistently.
	return run("sysctl", "-w", "net.ipv6.conf.all.disable_ipv6=1")
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

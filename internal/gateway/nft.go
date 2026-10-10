package gateway

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// RenderNFT produces the complete `inet pvm` table for the config:
// MASQUERADE out the uplink and out any exit tunnels, one DNAT per port
// map, DNS hijack (plain DNS is forced to the gateway resolver, DoT is
// refused so clients fall back to plain DNS), the fake-ip REDIRECT into
// the local transparent proxy, and a forward kill-switch: VMs routed
// via an exit may never talk to the uplink directly — except traffic
// whose destination is in the bypass set (direct CIDRs and IPs learned
// from direct-rule DNS answers), which must pass or direct rules would
// be dead for proxied VMs. The table is rebuilt atomically on every
// apply so stale rules can never linger; bypassIPs carries the current
// learned set so the rebuild preserves it.
func RenderNFT(c *Config, uplink, br string, bypassIPs []string) string {
	var b strings.Builder
	b.WriteString("destroy table inet pvm\n")
	b.WriteString("table inet pvm {\n")
	// bypass set lives at table level (sets inside chains are a syntax
	// error and abort the whole render); full rebuilds re-include the
	// learned IPs via bypassIPs.
	b.WriteString("  set bypass4 {\n")
	b.WriteString("    type ipv4_addr; flags interval;\n")
	if len(bypassIPs) > 0 {
		fmt.Fprintf(&b, "    elements = { %s }\n", strings.Join(bypassIPs, ", "))
	}
	b.WriteString("  }\n")
	fmt.Fprintf(&b, "  chain postrouting {\n")
	fmt.Fprintf(&b, "    type nat hook postrouting priority 100; policy accept;\n")
	fmt.Fprintf(&b, "    oifname %q masquerade\n", uplink)
	if len(c.Exits) > 0 {
		fmt.Fprintf(&b, "    oifname { %s } masquerade\n", quotedTunnels(c.Exits))
	}
	fmt.Fprintf(&b, "  }\n")
	b.WriteString("  chain prerouting {\n")
	b.WriteString("    type nat hook prerouting priority -100; policy accept;\n")
	for _, m := range c.Maps {
		proto := m.Proto
		if proto == "" {
			proto = "tcp"
		}
		fmt.Fprintf(&b, "    iifname %q %s dport %d dnat ip to %s:%d\n", uplink, proto, m.HostPort, m.VMIP, m.GuestPort)
	}
	// DNS hijack: whatever resolver a VM hard-codes, plain DNS terminates
	// on the gateway; DoT (853) is refused to force plain-DNS fallback.
	fmt.Fprintf(&b, "    iifname %q udp dport 53 dnat ip to %s\n", br, c.GwIP)
	fmt.Fprintf(&b, "    iifname %q tcp dport 53 dnat ip to %s\n", br, c.GwIP)
	fmt.Fprintf(&b, "    iifname %q udp dport 853 reject\n", br)
	fmt.Fprintf(&b, "    iifname %q tcp dport 853 reject\n", br)
	// fake-ip data plane: TCP goes to the local transparent proxy. All
	// UDP to fake IPs is refused (QUIC udp/443 retries over TCP; other
	// UDP has no proxy path and must fail fast instead of leaking into
	// the uplink or a tunnel with no reverse route). Bare "udp reject"
	// is an nft syntax error — match L4 via meta l4proto.
	fmt.Fprintf(&b, "    iifname %q ip daddr 198.18.0.0/16 meta l4proto udp reject\n", br)
	fmt.Fprintf(&b, "    iifname %q ip daddr 198.18.0.0/16 tcp dport != 53 redirect to :%d\n", br, fakeProxyPort)
	b.WriteString("  }\n")
	b.WriteString("  chain forward {\n")
	b.WriteString("    type filter hook forward priority 0; policy accept;\n")
	// kill-switch: exit-routed VMs may never reach the uplink directly
	// (fail closed when the tunnel is down) — but direct-rule traffic
	// policy-routed to the main table via the bypass set must pass.
	for _, v := range c.VMs {
		exitName, via := c.VMExits[v.Name]
		if !via || exitName == "" || exitName == "direct" {
			continue
		}
		// DHCP already pins each VM's MAC/IP pair; matching both makes
		// casual source-IP spoofing fail closed. A determined guest can
		// still forge both fields, so this is best-effort, not port isolation.
		mac := ""
		if v.MAC != "" {
			mac = " ether saddr " + v.MAC
		}
		fmt.Fprintf(&b, "    ip saddr %s%s oifname %q ip daddr @bypass4 accept\n", v.IP, mac, uplink)
		fmt.Fprintf(&b, "    ip saddr %s%s oifname %q drop\n", v.IP, mac, uplink)
	}
	b.WriteString("  }\n")
	b.WriteString("}\n")
	return b.String()
}

// quotedTunnels renders the tunnel interface set for every configured
// exit, named by the same sorted-index rule exitManager uses.
func quotedTunnels(exits map[string]ExitDef) string {
	names := make([]string, 0, len(exits))
	for n := range exits {
		names = append(names, n)
	}
	quoted := make([]string, len(names))
	for i := range names {
		quoted[i] = fmt.Sprintf("%q", tunnelIfName(i))
	}
	// sort the quoted names for stable output
	sort.Strings(quoted)
	return strings.Join(quoted, ", ")
}

// ApplyNFT feeds the ruleset to `nft -f`.
func ApplyNFT(ruleset string) error {
	f, err := os.CreateTemp("", "pvm-*.nft")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(ruleset); err != nil {
		f.Close()
		return err
	}
	f.Close()
	out, err := exec.Command("nft", "-f", f.Name()).CombinedOutput()
	if err != nil {
		return fmt.Errorf("nft -f: %v\n%s\nruleset:\n%s", err, out, ruleset)
	}
	return nil
}

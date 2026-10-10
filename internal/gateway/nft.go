package gateway

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// RenderNFT produces the complete `inet pvm` table for the config:
// MASQUERADE out the uplink plus one DNAT per port map. The table is
// rebuilt atomically on every apply so stale rules can never linger.
func RenderNFT(c *Config, uplink, br string) string {
	var b strings.Builder
	b.WriteString("destroy table inet pvm\n")
	b.WriteString("table inet pvm {\n")
	fmt.Fprintf(&b, "  chain postrouting {\n")
	fmt.Fprintf(&b, "    type nat hook postrouting priority 100; policy accept;\n")
	fmt.Fprintf(&b, "    oifname %q masquerade\n", uplink)
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
	b.WriteString("  }\n")
	b.WriteString("}\n")
	return b.String()
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

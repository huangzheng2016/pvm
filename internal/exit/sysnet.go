package exit

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// The agent owns exactly one nftables table and never touches anything else:
// install replaces only that table, remove deletes only that table.

const nftTable = "pvm_exit"

func nftRuleset(tunNames []string, cidrs []string) string {
	var ifSet, cidrSet string
	if len(tunNames) > 0 {
		quoted := make([]string, len(tunNames))
		for i, n := range tunNames {
			quoted[i] = fmt.Sprintf("%q", n)
		}
		ifSet = strings.Join(quoted, ", ")
	}
	parts := make([]string, len(cidrs))
	copy(parts, cidrs)
	cidrSet = strings.Join(parts, ", ")

	// chain names must avoid nftables keywords (e.g. "fwd" is reserved).
	var b strings.Builder
	b.WriteString("table inet " + nftTable + " {\n")
	b.WriteString("  chain pvm_forward {\n")
	b.WriteString("    type filter hook forward priority 0; policy accept;\n")
	if ifSet != "" {
		fmt.Fprintf(&b, "    iifname { %s } accept\n", ifSet)
		fmt.Fprintf(&b, "    oifname { %s } ct state { established, related } accept\n", ifSet)
	}
	b.WriteString("  }\n")
	b.WriteString("  chain pvm_nat {\n")
	b.WriteString("    type nat hook postrouting priority srcnat; policy accept;\n")
	var sb strings.Builder
	fmt.Fprintf(&sb, "    ip saddr { %s } ", cidrSet)
	for _, n := range tunNames {
		fmt.Fprintf(&sb, "oifname != %q ", n)
	}
	sb.WriteString("masquerade\n")
	b.WriteString(sb.String())
	b.WriteString("  }\n")
	b.WriteString("}\n")
	return b.String()
}

// NFTInstall creates/replaces our table. Best-effort cleanup of any
// previous version first.
func NFTInstall(tunNames []string, cidrs []string) error {
	_ = NFTRemove()
	return runNFT(nftRuleset(tunNames, cidrs))
}

// NFTRemove deletes our table only.
func NFTRemove() error {
	_, err := exec.Command("nft", "delete", "table", "inet", nftTable).CombinedOutput()
	if err != nil && !strings.Contains(string(errOutput(err)), "No such file") {
		return fmt.Errorf("nft delete table: %w", err)
	}
	return nil
}

func errOutput(err error) []byte {
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.Stderr
	}
	return nil
}

func runNFT(script string) error {
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nft -f: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ConfigureTUN assigns addr/cidr to the interface and brings it up (Linux).
func ConfigureTUN(name, addrWithMask string) error {
	if out, err := exec.Command("ip", "addr", "replace", addrWithMask, "dev", name).CombinedOutput(); err != nil {
		return fmt.Errorf("ip addr replace: %v: %s", err, out)
	}
	if out, err := exec.Command("ip", "link", "set", "dev", name, "up").CombinedOutput(); err != nil {
		return fmt.Errorf("ip link up: %v: %s", err, out)
	}
	return nil
}

// EnableIPForward turns on IPv4 forwarding at runtime and persists it.
func EnableIPForward() error {
	if out, err := exec.Command("sysctl", "-w", "net.ipv4.ip_forward=1").CombinedOutput(); err != nil {
		return fmt.Errorf("sysctl ip_forward: %v: %s", err, out)
	}
	if _, err := os.Stat("/etc/sysctl.d"); err == nil {
		_ = os.WriteFile("/etc/sysctl.d/99-pvm-exit.conf", []byte("net.ipv4.ip_forward=1\n"), 0o644)
	}
	return nil
}

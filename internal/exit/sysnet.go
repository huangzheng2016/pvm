package exit

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
)

// The agent owns exactly one nftables table and never touches anything else:
// install replaces only that table, remove deletes only that table.

const nftTable = "pvm_exit"

// nftTableFor namespaces the table by tun prefix so coexisting instances
// (e.g. a test instance with --tun-prefix) never delete each other's rules.
func nftTableFor(tunNames []string) string {
	prefix := "pvmexit"
	if len(tunNames) > 0 {
		prefix = tunNames[0]
		if strings.HasSuffix(prefix, "0") || strings.HasSuffix(prefix, "1") {
			prefix = prefix[:len(prefix)-1]
		}
	}
	if prefix == "pvmexit" {
		return nftTable
	}
	return nftTable + "_" + prefix
}

func nftRuleset(tunNames []string, cidrs []string, allow ...*net.IPNet) string {
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
	b.WriteString("table inet " + nftTableFor(tunNames) + " {\n")
	b.WriteString("  chain pvm_input {\n")
	b.WriteString("    type filter hook input priority -10; policy accept;\n")
	for i, name := range tunNames {
		if i < len(cidrs) {
			ip := tunnelServerIP(cidrs[i])
			fmt.Fprintf(&b, "    iifname %q ip daddr %s tcp dport { %d, %d } accept\n", name, ip, DomainConnectPort, SpeedPort)
			fmt.Fprintf(&b, "    iifname %q ip daddr %s udp dport %d accept\n", name, ip, UDPProbePort)
			fmt.Fprintf(&b, "    iifname %q ip daddr %s icmp type echo-request accept\n", name, ip)
		}
		for _, n := range allow {
			fmt.Fprintf(&b, "    iifname %q ip daddr %s accept\n", name, n)
		}
		fmt.Fprintf(&b, "    iifname %q drop\n", name)
	}
	b.WriteString("  }\n")
	b.WriteString("  chain pvm_forward {\n")
	b.WriteString("    type filter hook forward priority -10; policy accept;\n")
	if ifSet != "" {
		for _, n := range allow {
			fmt.Fprintf(&b, "    iifname { %s } ip daddr %s accept\n", ifSet, n)
		}
		fmt.Fprintf(&b, "    iifname { %s } ip daddr { %s } drop\n", ifSet, strings.Join(blockedEgressCIDRs, ", "))
		fmt.Fprintf(&b, "    iifname { %s } meta nfproto ipv6 drop\n", ifSet)
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

func NFTInstall(tunNames []string, cidrs []string, allow ...*net.IPNet) error {
	// Prefer the atomic destroy+recreate, but "destroy table" needs nftables
	// >= 1.0.7 and Debian 12 ships 1.0.6. A failed nft -f applies nothing,
	// so falling back to delete-then-create is safe.
	err := runNFT("destroy table inet " + nftTableFor(tunNames) + "\n" + nftRuleset(tunNames, cidrs, allow...))
	if err == nil {
		return nil
	}
	if rmErr := NFTRemove(tunNames); rmErr != nil {
		return err
	}
	return runNFT(nftRuleset(tunNames, cidrs, allow...))
}

func NFTRemove(tunNames []string) error {
	out, err := exec.Command("nft", "delete", "table", "inet", nftTableFor(tunNames)).CombinedOutput()
	// ExitError.Stderr is only populated by Cmd.Output, never by
	// CombinedOutput: read the captured bytes for the idempotency check.
	if err != nil && !strings.Contains(string(out), "No such file") {
		return fmt.Errorf("nft delete table: %v: %s", err, strings.TrimSpace(string(out)))
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

func ConfigureTUN(name, addrWithMask string) error {
	if out, err := exec.Command("ip", "addr", "replace", addrWithMask, "dev", name).CombinedOutput(); err != nil {
		return fmt.Errorf("ip addr replace: %v: %s", err, out)
	}
	if out, err := exec.Command("ip", "link", "set", "dev", name, "up").CombinedOutput(); err != nil {
		return fmt.Errorf("ip link up: %v: %s", err, out)
	}
	return nil
}

func EnableIPForward() error {
	if out, err := exec.Command("sysctl", "-w", "net.ipv4.ip_forward=1").CombinedOutput(); err != nil {
		return fmt.Errorf("sysctl ip_forward: %v: %s", err, out)
	}
	if _, err := os.Stat("/etc/sysctl.d"); err == nil {
		_ = os.WriteFile("/etc/sysctl.d/99-pvm-exit.conf", []byte("net.ipv4.ip_forward=1\n"), 0o644)
	}
	return nil
}

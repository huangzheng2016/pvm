package exit

import (
	"fmt"
	"os/exec"
	"strings"

	"golang.zx2c4.com/wireguard/tun"
)

type nftBackend struct{}

func (nftBackend) name() string { return NATNftables }

func (nftBackend) newTun(tunName string, mtu int) (tun.Device, error) {
	return netfilterTun(tunName, mtu)
}

func (nftBackend) setup(tunNames []string, cidrs []string) error {
	if err := EnableIPForward(); err != nil {
		return err
	}
	return NFTInstall(tunNames, cidrs)
}

func (nftBackend) cleanup(tunNames []string, _ []string) error {
	if len(tunNames) == 0 {
		// setup never ran (e.g. startup failed before transports existed):
		// nothing is ours to remove, and deleting the default table would
		// clobber a coexisting production instance.
		return nil
	}
	return NFTRemove(tunNames)
}

// iptablesBackend installs MASQUERADE + FORWARD accepts, every rule tagged
// with `-m comment --comment pvm-exit` so cleanup deletes exactly ours.
type iptablesBackend struct{}

const iptablesComment = "pvm-exit"

func (iptablesBackend) name() string { return NATIptables }

func (iptablesBackend) newTun(tunName string, mtu int) (tun.Device, error) {
	return netfilterTun(tunName, mtu)
}

// iptablesRules renders the rule set as (table, chain, spec) triples; spec is
// a space-separated argument list (no quoting inside).
func iptablesRules(tunNames []string, cidrs []string) [][3]string {
	var rules [][3]string
	for _, cidr := range cidrs {
		rules = append(rules, [3]string{"nat", "POSTROUTING",
			fmt.Sprintf("-s %s -m comment --comment %s -j MASQUERADE", cidr, iptablesComment)})
	}
	for _, name := range tunNames {
		rules = append(rules, [3]string{"filter", "FORWARD",
			fmt.Sprintf("-i %s -m comment --comment %s -j ACCEPT", name, iptablesComment)})
		rules = append(rules, [3]string{"filter", "FORWARD",
			fmt.Sprintf("-o %s -m conntrack --ctstate ESTABLISHED,RELATED -m comment --comment %s -j ACCEPT", name, iptablesComment)})
	}
	return rules
}

func runIptables(args ...string) error {
	cmd := exec.Command("iptables", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("iptables %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (iptablesBackend) setup(tunNames []string, cidrs []string) error {
	if err := EnableIPForward(); err != nil {
		return err
	}
	for _, r := range iptablesRules(tunNames, cidrs) {
		args := append([]string{"-t", r[0], "-A", r[1]}, strings.Fields(r[2])...)
		if err := runIptables(args...); err != nil {
			return err
		}
	}
	return nil
}

func (iptablesBackend) cleanup(tunNames []string, cidrs []string) error {
	if len(tunNames) == 0 && len(cidrs) == 0 {
		return nil
	}
	return iptablesRemoveMarked(iptablesComment, tunNames, cidrs)
}

// iptablesRemoveMarked deletes every rule carrying our comment, parsed from
// iptables-save so removal matches whatever is actually installed.
func iptablesRemoveMarked(comment string, tunNames, cidrs []string) error {
	out, err := exec.Command("iptables-save").CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables-save: %v: %s", err, strings.TrimSpace(string(out)))
	}
	for _, d := range parseMarkedDeletes(string(out), comment, tunNames, cidrs) {
		if err := runIptables(d...); err != nil {
			return err
		}
	}
	return nil
}

// parseMarkedDeletes turns iptables-save output into delete command argument
// vectors for rules carrying the comment.
func parseMarkedDeletes(saveOutput, comment string, tunNames, cidrs []string) [][]string {
	ours := func(spec string) bool {
		for _, n := range tunNames {
			if strings.Contains(spec, "-i "+n) || strings.Contains(spec, "-o "+n) {
				return true
			}
		}
		for _, c := range cidrs {
			if strings.Contains(spec, "-s "+c) {
				return true
			}
		}
		return false
	}
	var out [][]string
	table := ""
	for line := range strings.Lines(saveOutput) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "*") {
			table = strings.TrimPrefix(line, "*")
			continue
		}
		spec, ok := strings.CutPrefix(line, "-A ")
		if !ok {
			continue
		}
		// iptables-save quotes comments ("pvm-exit"); strip quotes for
		// matching and for the delete argument vector
		spec = strings.ReplaceAll(spec, `"`, "")
		if !strings.Contains(spec, "--comment "+comment) || !ours(spec) {
			continue
		}
		fields := strings.Fields(spec)
		if len(fields) == 0 {
			continue
		}
		out = append(out, append([]string{"-t", table, "-D"}, fields...))
	}
	return out
}

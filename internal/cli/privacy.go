package cli

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/huangzheng2016/pvm/internal/cluster"
	"github.com/huangzheng2016/pvm/internal/config"
	"github.com/huangzheng2016/pvm/internal/exit"
	"github.com/huangzheng2016/pvm/internal/gw/rules"
	"github.com/huangzheng2016/pvm/internal/keys"
	"github.com/huangzheng2016/pvm/internal/paths"
	"github.com/huangzheng2016/pvm/internal/vm"
)

func checkExistingKey(path string) error {
	if _, err := os.Lstat(path); err != nil {
		return err
	}
	return keys.CheckPermissions(path)
}

// Privacy checks read local snapshots only; they neither start the daemon nor
// probe destinations. Details deliberately omit paths, names and credentials.
func privacyChecks(p *paths.Paths, name string) []auditCheck {
	var checks []auditCheck
	add := func(name, status, detail string) { checks = append(checks, auditCheck{name, status, detail}) }
	if err := p.CheckPermissions(); err != nil {
		add("state_permissions", "fail", "state directory missing, unreadable, or not private; check owner and mode 0700")
	} else {
		add("state_permissions", "ok", "state directories are private")
	}
	cfg, err := config.Load(p.ConfigFile())
	if err != nil {
		add("update_check", "fail", "cannot parse local configuration")
	} else {
		enabled := (cfg.UpdateCheck || os.Getenv("PVM_UPDATE_CHECK") == "1") && os.Getenv("PVM_NO_UPDATE_CHECK") != "1"
		if enabled {
			add("update_check", "warn", "host update checks are enabled and contact GitHub")
		} else {
			add("update_check", "ok", "automatic host update checks are disabled")
		}
	}
	spec, err := vm.Load(p, name)
	if err != nil {
		add("vm", "fail", "VM metadata missing or unreadable")
		return checks
	}
	if err := checkExistingKey(spec.SSHKeyPath(p)); err != nil {
		add("ssh_key", "fail", "management private key missing or permissions unsafe; restore the key or restrict it to its owner")
	} else if spec.SSHKeyName == "" {
		add("ssh_key", "warn", "uses a shared management key, which can correlate guests")
	} else {
		add("ssh_key", "ok", "uses a dedicated management key")
	}
	switch spec.ShareMode {
	case vm.ShareNone:
		add("share", "ok", "automatic host share disabled")
	case vm.SharePrivate:
		add("share", "ok", "private per-VM share")
	default:
		add("share", "warn", "global writable share can transmit data between guests")
	}
	ro := 0
	for _, mount := range spec.Mounts {
		if mount.ReadOnly {
			ro++
		}
	}
	status := "ok"
	if len(spec.Mounts) != ro {
		status = "warn"
	}
	add("mounts", status, fmt.Sprintf("%d explicit mounts, %d read-only; source paths withheld", len(spec.Mounts), ro))
	store, err := cluster.LoadStore(p.Clusters)
	if err != nil {
		add("route", "fail", "cluster state is unreadable")
		return checks
	}
	cl := store.Get(spec.ClusterID)
	if cl == nil {
		add("route", "warn", "no managed cluster route")
		return checks
	}
	via, ok := cl.VMExits[spec.Name]
	if !ok {
		via = cl.DefaultExit
	}
	if via == "" || via == "direct" {
		add("default_route", "warn", "unmatched traffic uses host egress")
	} else if _, err := exit.LoadEntry(filepath.Join(p.Home, "exits"), via); err != nil {
		add("default_route", "fail", "selected exit is missing or unreadable")
	} else {
		add("default_route", "ok", "unmatched traffic selects an exit; name withheld")
	}
	exceptions := 0
	for _, rule := range cl.Rules {
		if rule.Action == rules.Direct || rule.Action == rules.Internal {
			exceptions++
		}
	}
	status = "ok"
	if exceptions > 0 {
		status = "warn"
	}
	add("route_exceptions", status, fmt.Sprintf("%d explicit direct/internal rules; patterns withheld", exceptions))
	add("runtime", "skipped", "local configuration only; live forwarding and tunnel-loss behavior not tested")
	return checks
}

func cmdPrivacyAudit(name string) int {
	if err := vm.ValidateName(name); err != nil {
		return fatalf("invalid VM name")
	}
	p, err := paths.Default()
	if err != nil {
		return fatalf("cannot locate pvm state")
	}
	return printAudit(os.Stdout, privacyChecks(p, name))
}

func CmdDoctor(args []string) int {
	if len(args) != 0 {
		return fatalf("usage: pvm doctor")
	}
	p, err := paths.Default()
	if err != nil {
		return fatalf("cannot locate pvm state")
	}
	checks := []auditCheck{}
	if err := p.CheckPermissions(); err != nil {
		checks = append(checks, auditCheck{"permissions", "fail", "check state directory ownership and mode 0700"})
	}
	specs, err := vm.ListAll(p)
	if err != nil {
		checks = append(checks, auditCheck{"metadata", "fail", "one or more VM metadata files are unreadable"})
	}
	store, err := cluster.LoadStore(p.Clusters)
	if err != nil {
		checks = append(checks, auditCheck{"clusters", "fail", "cluster state is unreadable"})
	}
	known := map[string]*vm.Spec{}
	missingDisk, badKeys, badMembers := 0, 0, 0
	for _, spec := range specs {
		known[spec.Name] = spec
		if _, err := os.Stat(spec.Disk(p)); err != nil {
			missingDisk++
		}
		if err := checkExistingKey(spec.SSHKeyPath(p)); err != nil {
			badKeys++
		}
		if store != nil && spec.Role != "gateway" && spec.ClusterID != 0 {
			cl := store.Get(spec.ClusterID)
			if cl == nil || cl.VMs[spec.Name] == nil || cl.VMs[spec.Name].IP != spec.IP || cl.VMs[spec.Name].MAC != spec.MAC {
				badMembers++
			}
		}
	}
	if store != nil {
		for _, cl := range store.List() {
			for name := range cl.VMs {
				if known[name] == nil {
					badMembers++
				}
			}
			for _, m := range cl.Maps {
				if member := cl.VMs[m.VM]; member == nil || member.IP != m.VMIP {
					badMembers++
				}
			}
		}
	}
	for _, item := range []struct {
		name  string
		count int
	}{{"disks", missingDisk}, {"keys", badKeys}, {"membership", badMembers}} {
		status := "ok"
		if item.count != 0 {
			status = "fail"
		}
		checks = append(checks, auditCheck{item.name, status, fmt.Sprintf("%d inconsistencies; identifiers withheld", item.count)})
	}
	checks = append(checks, windowsDoctorChecks(p)...)
	checks = append(checks, auditCheck{"runtime", "skipped", "read-only disk snapshot; active relays and applied gateway rules are not inspected"})
	return printAudit(os.Stdout, checks)
}

func routeExplanation(cl *cluster.Cluster, name, destination string) (string, error) {
	eng, err := rules.NewEngine("", rules.DefaultInternal())
	if err != nil {
		return "", err
	}
	for _, rule := range cl.Rules {
		if err := eng.Add(rule); err != nil {
			return "", err
		}
	}
	ip := net.ParseIP(destination)
	domain := destination
	if ip != nil {
		domain = ""
	}
	if d, ok := eng.Match(domain, ip); ok {
		if d.Internal {
			return "configured route: cluster-local (built-in internal suffix)", nil
		}
		if d.Action == rules.Proxy {
			return "configured route: proxy (explicit rule; exit name withheld)", nil
		}
		return "configured route: direct (explicit rule; host egress)", nil
	}
	via, ok := cl.VMExits[name]
	source := "cluster default"
	if ok {
		source = "VM override"
	} else {
		via = cl.DefaultExit
	}
	if via == "" || via == "direct" {
		return "configured route: direct (" + source + "; host egress)", nil
	}
	return "configured route: proxy (" + source + "; exit name withheld)", nil
}

func cmdRouteExplain(args []string) int {
	if len(args) != 2 {
		return fatalf("usage: pvm route explain <vm> <domain-or-ip>")
	}
	if err := vm.ValidateName(args[0]); err != nil {
		return fatalf("invalid VM name")
	}
	p, err := paths.Default()
	if err != nil {
		return fatalf("cannot locate pvm state")
	}
	spec, err := vm.Load(p, args[0])
	if err != nil {
		return fatalf("cannot read VM metadata")
	}
	store, err := cluster.LoadStore(p.Clusters)
	if err != nil {
		return fatalf("cannot read cluster state")
	}
	cl := store.Get(spec.ClusterID)
	if cl == nil {
		return fatalf("VM has no managed cluster route")
	}
	result, err := routeExplanation(cl, spec.Name, args[1])
	if err != nil {
		return fatalf("invalid stored routing rules")
	}
	fmt.Println(result)
	fmt.Println("Local configuration only; no DNS query or connection made. Domain results do not include CIDR rules for unresolved addresses or cached direct-domain IPs.")
	return 0
}

package cli

import (
	"fmt"
	"strings"

	"github.com/huangzheng2016/pvm/internal/daemon"
	"github.com/huangzheng2016/pvm/internal/vm"
)

// CmdSSHConfig prints an OpenSSH config block for a VM (including a
// ProxyCommand hop through the cluster gateway). It is print-only and never
// writes to ~/.ssh.
func CmdSSHConfig(args []string) int {
	args = scanJSONFlag(args)
	if len(args) != 1 {
		return failErr(fmt.Errorf("usage: pvm ssh-config <vm>"))
	}
	p, err := mustPaths()
	if err != nil {
		return failErr(err)
	}
	spec, err := vm.Load(p, args[0])
	if err != nil {
		return failErr(err)
	}
	lines := []string{
		"Host " + spec.Name,
		"    User " + spec.User,
		"    IdentityFile " + sshConfigQuote(spec.SSHKeyPath(p)),
		"    IdentitiesOnly yes",
		"    IdentityAgent none",
		"    ForwardAgent no",
		"    StrictHostKeyChecking no",
		"    UserKnownHostsFile /dev/null",
		"    LogLevel ERROR",
	}
	if spec.Role == "gateway" || spec.ClusterID == 0 {
		lines = append(lines, fmt.Sprintf("    HostName 127.0.0.1\n    Port %d", spec.SSHPort))
	} else {
		c, err := daemon.Ensure(p)
		if err != nil {
			return failErr(err)
		}
		var info vmInfoView
		if err := c.Get("/v1/vms/"+spec.Name, &info); err != nil {
			return failErr(err)
		}
		if spec.IP != "" {
			lines = append(lines, "    HostName "+spec.IP)
		}
		if info.GatewaySSH > 0 {
			gwKey := p.SSHKey()
			if info.GatewaySSHKeyName != "" {
				gwKey = p.VMSSHKey(info.GatewaySSHKeyName)
			}
			gwUser := info.GatewayUser
			if gwUser == "" {
				gwUser = "pvmgw"
			}
			proxy := fmt.Sprintf("ssh -F /dev/null -o IdentitiesOnly=yes -o IdentityAgent=none -o ForwardAgent=no -i %s -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -W %%h:%%p -p %d %s@127.0.0.1",
				vm.ProxyCommandQuote(gwKey), info.GatewaySSH, gwUser)
			lines = append(lines, "    ProxyCommand "+proxy)
		}
	}
	out := strings.Join(lines, "\n") + "\n"
	if jsonOutput {
		return failErr(writeJSONOut(map[string]any{"status": "completed", "data": map[string]string{"vm": spec.Name, "config": out}}))
	}
	fmt.Print(out)
	return 0
}

func sshConfigQuote(p string) string {
	if !strings.ContainsAny(p, " \t\"") {
		return p
	}
	return `"` + strings.ReplaceAll(p, `"`, `\"`) + `"`
}

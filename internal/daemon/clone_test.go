package daemon

import (
	"strings"
	"testing"

	"github.com/huangzheng2016/pvm/internal/vm"
)

func TestIdentityResetScript(t *testing.T) {
	s := identityResetScript(&vm.Spec{Name: "dev2", Image: "ubuntu:24.04"})
	for _, want := range []string{
		"hostnamectl set-hostname dev2",
		"rm -f /etc/machine-id /var/lib/dbus/machine-id",
		"systemd-machine-id-setup",
		"rm -f /etc/ssh/ssh_host_*",
		"ssh-keygen -A",
		"systemctl restart ssh.socket ssh.service",
		"netplan/50-cloud-init.yaml",
		"name: en*", // MAC-agnostic, so future clones boot with network
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script missing %q\nscript: %s", want, s)
		}
	}
	// The script must run under set -e so the tolerated ssh restart at the
	// end can never mask a real failure earlier in the chain as a false
	// success.
	if !strings.HasPrefix(s, "set -e") {
		t.Error("script must start with set -e")
	}
}

func TestIdentityResetScriptSudoConditional(t *testing.T) {
	// The agent channel runs as root; the script must elevate only when
	// not already root.
	s := identityResetScript(&vm.Spec{Name: "a", Image: "ubuntu:24.04"})
	if !strings.Contains(s, `[ "$(id -u)" -ne 0 ] && SUDO='sudo'`) {
		t.Errorf("script must gate sudo on id -u\nscript: %s", s)
	}
	if strings.Contains(s, " sudo ") || strings.HasPrefix(s, "sudo ") {
		t.Errorf("bare sudo would fail as root\nscript: %s", s)
	}
}

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
		"rm -f /root/.claude.json",
		`rm -rf "$home/.claude"`,
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

func TestAlpineIdentityResetScript(t *testing.T) {
	s := identityResetScript(&vm.Spec{Name: "dev3", Image: "alpine:3.21"})
	for _, want := range []string{
		"tee /etc/hostname",
		"hostname -F /etc/hostname",
		"rm -f /etc/machine-id /var/lib/dbus/machine-id",
		"random/uuid",
		"rm -f /etc/ssh/ssh_host_*",
		"ssh-keygen -A",
		"rc-service sshd restart",
		"rm -f /root/.claude.json",
		`rm -rf "$home/.claude"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("alpine script missing %q\nscript: %s", want, s)
		}
	}
	for _, banned := range []string{"netplan", "hostnamectl", "systemd", "systemctl"} {
		if strings.Contains(s, banned) {
			t.Errorf("alpine script must not use %s\nscript: %s", banned, s)
		}
	}
}

func TestIdentityResetScriptSudoConditional(t *testing.T) {
	// The agent channel runs as root and alpine's sudoers refuses root;
	// both scripts must elevate only when not already root.
	for _, spec := range []*vm.Spec{
		{Name: "a", Image: "ubuntu:24.04"},
		{Name: "b", Image: "alpine:3.21"},
	} {
		s := identityResetScript(spec)
		if !strings.Contains(s, `[ "$(id -u)" -ne 0 ] && SUDO='sudo'`) {
			t.Errorf("%s: script must gate sudo on id -u\nscript: %s", spec.Image, s)
		}
		if strings.Contains(s, " sudo ") || strings.HasPrefix(s, "sudo ") {
			t.Errorf("%s: bare sudo would fail as root on alpine\nscript: %s", spec.Image, s)
		}
	}
}

package daemon

import (
	"strings"
	"testing"
)

func TestIdentityResetScript(t *testing.T) {
	s := identityResetScript("dev2")
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
	// The tolerated-failure restart must be separated by ';' so a restart
	// hiccup cannot mask a real failure earlier in the chain.
	if strings.Contains(s, "ssh-keygen -A && sudo systemctl restart") {
		t.Error("restart chained with && would let '|| true' mask real failures")
	}
}

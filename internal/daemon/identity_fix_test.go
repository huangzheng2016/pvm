package daemon

import (
	"strings"
	"testing"
)

func TestSanitizeTZ(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"America/Los_Angeles\n", "America/Los_Angeles"},
		{"\n\nAsia/Shanghai\n", "Asia/Shanghai"},
		{"", "UTC"},
		{"   \n", "UTC"},
		{"$(rm -rf /)\n", "UTC"},
		{"America/Los_Angeles; reboot\n", "UTC"},
	} {
		if got := sanitizeTZ(tc.in); got != tc.want {
			t.Errorf("sanitizeTZ(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAgentOpenRCBackgrounds(t *testing.T) {
	for _, want := range []string{
		`command_background="yes"`,
		`pidfile="/run/pvm-agent.pid"`,
		"need net",
	} {
		if !strings.Contains(agentOpenRC, want) {
			t.Errorf("openrc script missing %q\nscript: %s", want, agentOpenRC)
		}
	}
	// A foreground exec without command_background blocks OpenRC's
	// runlevel and starves later services (the sshd-stopped bug); the old
	// start_pre sshd hack must stay gone so the root cause can't return
	// masked.
	if strings.Contains(agentOpenRC, "start_pre") {
		t.Error("start_pre hack is back; fix the service, not the symptom")
	}
}

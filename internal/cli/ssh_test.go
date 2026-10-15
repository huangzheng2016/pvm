package cli

import "testing"

func TestSSHWantTTY(t *testing.T) {
	if !sshWantsTTY(nil, false, false) {
		t.Fatal("interactive login must allocate a TTY")
	}
	if !sshWantsTTY([]string{"codex"}, true, true) {
		t.Fatal("interactive command from a terminal must allocate a TTY")
	}
	if sshWantsTTY([]string{"codex"}, true, false) {
		t.Fatal("piped stdout must not allocate a TTY")
	}
	if sshWantsTTY([]string{"codex"}, false, true) {
		t.Fatal("piped stdin must not allocate a TTY")
	}
}

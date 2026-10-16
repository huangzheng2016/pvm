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

func TestSSHLoginCommand(t *testing.T) {
	for _, tc := range []struct {
		tty  bool
		want string
	}{
		{false, `exec "${SHELL:-/bin/sh}" -lc 'claude --version'`},
		{true, `exec "${SHELL:-/bin/sh}" -lic 'claude --version'`},
	} {
		if got := sshLoginCommand([]string{"claude", "--version"}, tc.tty); got != tc.want {
			t.Errorf("sshLoginCommand(tty=%v) = %q, want %q", tc.tty, got, tc.want)
		}
	}
}

func TestSSHLoginCommandQuotes(t *testing.T) {
	got := sshLoginCommand([]string{`printf '%s' "a b"`}, false)
	want := `exec "${SHELL:-/bin/sh}" -lc 'printf '"'"'%s'"'"' "a b"'`
	if got != want {
		t.Fatalf("quoted command = %q, want %q", got, want)
	}
}

package cli

import "testing"

func TestInteractiveShell(t *testing.T) {
	for _, command := range []string{"bash", "/bin/bash", "sh", "zsh", "fish"} {
		if !interactiveShell(command) {
			t.Errorf("%q should be recognized as an interactive shell", command)
		}
	}
	for _, command := range []string{"", "echo", "codex", "bash.exe"} {
		if interactiveShell(command) {
			t.Errorf("%q should not be recognized as an interactive shell", command)
		}
	}
}

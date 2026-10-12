package qemu

import (
	"os/exec"
	"testing"
)

// ps lstart is locale-sensitive: under zh_CN the date has fewer
// whitespace fields, which used to make IdentityOf report a running
// process as "process not found" and get it killed.
func TestIdentityOfAnyLocale(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for _, locale := range []string{"C", "zh_CN.UTF-8"} {
		t.Setenv("LANG", locale)
		t.Setenv("LC_ALL", locale)
		id, err := IdentityOf(cmd.Process.Pid)
		if err != nil {
			t.Fatalf("locale %s: IdentityOf: %v", locale, err)
		}
		if !id.Alive() {
			t.Fatalf("locale %s: alive process reported dead", locale)
		}
	}
	cmd.Process.Kill()
	cmd.Wait()
	if id, err := IdentityOf(cmd.Process.Pid); err == nil && id.Alive() {
		t.Fatal("dead process reported alive")
	}
}

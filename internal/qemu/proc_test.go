package qemu

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ps lstart is locale-sensitive; localized dates can have fewer whitespace fields.
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
		if strings.HasSuffix(id.Started, " "+id.State) {
			t.Fatalf("process state included in Started: %q", id.Started)
		}
	}
	cmd.Process.Kill()
	cmd.Wait()
	if id, err := IdentityOf(cmd.Process.Pid); err == nil && id.Alive() {
		t.Fatal("dead process reported alive")
	}
}

func TestIdentityOfMissingProcess(t *testing.T) {
	id, err := IdentityOf(999999)
	if err == nil {
		t.Fatal("missing process must not have an identity")
	}
	if id != nil && id.Alive() {
		t.Fatal("missing process reported alive")
	}
	if err != errNoProc {
		t.Fatalf("error = %v, want errNoProc", err)
	}
}

func TestStartedWithoutState(t *testing.T) {
	legacy := "Oct 14 17:21:51 2026 Ss"
	current := "Oct 14 17:21:51 2026"
	if startedWithoutState(legacy) != current {
		t.Fatalf("legacy started = %q, want %q", startedWithoutState(legacy), current)
	}
	if startedWithoutState(current) != current {
		t.Fatalf("current started changed: %q", startedWithoutState(current))
	}
}

func TestIdentityOfZombieIsNotAlive(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	var id *ProcIdentity
	for i := 0; i < 50; i++ {
		candidate, err := IdentityOf(cmd.Process.Pid)
		if err == nil {
			id = candidate
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if id == nil {
		t.Fatal("process did not become a zombie")
	}
	if id.Alive() {
		t.Fatalf("zombie reported alive: state=%q", id.State)
	}
}

func TestKillVerifiedRefusesProbeFailure(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	id, err := IdentityOf(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte("#!/bin/sh\necho unavailable\nexit 2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if err := killVerified(&RunState{PID: id.PID, Ident: id}, syscall.SIGTERM); err == nil {
		t.Fatal("must refuse signal when ps fails")
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("process was signalled despite failed verification: %v", err)
	}
}

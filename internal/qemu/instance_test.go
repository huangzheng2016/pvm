package qemu

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huangzheng2016/pvm/internal/diaglog"
)

func TestRunningDistinguishesCorruptState(t *testing.T) {
	dir := t.TempDir()
	in := &Instance{StatePath: filepath.Join(dir, "run.json")}

	st, err := in.Running()
	if err != nil || st != nil {
		t.Fatalf("missing state should be a clean not-running result: st=%v err=%v", st, err)
	}

	if err := os.WriteFile(in.StatePath, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err = in.Running()
	if err == nil || st != nil {
		t.Fatalf("corrupt state must be an explicit error: st=%v err=%v", st, err)
	}
	if err := in.Start(); err == nil || !strings.Contains(err.Error(), "corrupted state file") {
		t.Fatalf("Start must refuse corrupted state with repair guidance, got %v", err)
	}
	if err := in.Stop(); err == nil {
		t.Fatal("Stop must not silently treat corrupt state as stopped")
	}
	if _, statErr := os.Stat(in.StatePath); statErr != nil {
		t.Fatalf("state file must be preserved for diagnosis: %v", statErr)
	}
}

func TestStartRotatesOversizedLogs(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-qemu")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nsleep 2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	console := filepath.Join(dir, "console.log")
	if err := os.WriteFile(console, bytes.Repeat([]byte("x"), diaglog.DefaultConsoleMaxBytes+16), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(console+".qemu", bytes.Repeat([]byte("y"), diaglog.DefaultQEMULogMaxBytes+16), 0o600); err != nil {
		t.Fatal(err)
	}
	in := &Instance{
		Cfg: VMConfig{
			CPUs: 1, MemMB: 128, DiskPath: filepath.Join(dir, "d.qcow2"), SeedPath: filepath.Join(dir, "seed.iso"),
			VarsPath: filepath.Join(dir, "vars.fd"),
			QMPSock:  filepath.Join(dir, "q.sock"), SerialSock: filepath.Join(dir, "s.sock"),
			ConsoleLog: console, Accel: AccelTCG, Network: Network{Mode: NetSlirp},
		},
		StatePath: filepath.Join(dir, "run.json"),
		Toolchain: &Toolchain{System: fake, FirmwareDir: dir},
	}
	if err := in.Start(); err != nil {
		t.Logf("fake qemu start returned: %v", err)
	}
	if _, err := os.Stat(console + ".1"); err != nil {
		t.Fatalf("oversized console.log was not rotated before start: %v", err)
	}
	if _, err := os.Stat(console + ".qemu.1"); err != nil {
		t.Fatalf("oversized console.log.qemu was not rotated before start: %v", err)
	}
}

func TestRunningRejectsInvalidIdentity(t *testing.T) {
	in := &Instance{StatePath: filepath.Join(t.TempDir(), "run.json")}
	for _, state := range []string{`{}`, `{"pid":123}`, `{"pid":123,"ident":{"pid":124,"comm":"qemu","started":"Oct 26 12:00:00 2026"}}`} {
		if err := os.WriteFile(in.StatePath, []byte(state), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := in.Running(); err == nil {
			t.Fatalf("invalid identity accepted: %s", state)
		}
	}
}

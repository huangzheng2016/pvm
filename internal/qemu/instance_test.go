package qemu

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

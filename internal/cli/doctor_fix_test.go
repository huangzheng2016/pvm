package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huangzheng2016/pvm/internal/paths"
)

func TestDoctorFixSafeRepairsModesAndStaleTempsOnly(t *testing.T) {
	p := paths.New(t.TempDir())
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p.Runtime, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(p.Images, "ubuntu-24.04-arm64.qcow2.tmp")
	if err := os.WriteFile(stale, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	keyPath := p.SSHKey()
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("private-key-fixture"), 0o600); err != nil {
		t.Fatal(err)
	}

	n, err := doctorFixSafe(p)
	if err != nil || n < 2 {
		t.Fatalf("doctorFixSafe = %d, %v", n, err)
	}
	if fi, _ := os.Stat(p.Runtime); fi.Mode().Perm() != 0o700 {
		t.Fatalf("runtime mode = %v", fi.Mode())
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale temp image was not removed")
	}
	if b, err := os.ReadFile(keyPath); err != nil || string(b) != "private-key-fixture" {
		t.Fatal("doctor --fix touched private key")
	}
	if err := p.CheckPermissions(); err != nil {
		t.Fatalf("permissions still failing: %v", err)
	}
}

func TestSanitizeTextStripsBidiAndControl(t *testing.T) {
	got := sanitizeText("ok\x1b[31m\u202eevil\u200b.txt")
	if strings.ContainsAny(got, "\x1b\u202e\u200b") || !strings.Contains(got, "?") {
		t.Fatalf("sanitizeText = %q", got)
	}
}

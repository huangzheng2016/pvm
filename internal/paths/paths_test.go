package paths

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/huangzheng2016/pvm/internal/native"
)

func TestPVMHomeOverride(t *testing.T) {
	t.Setenv("PVM_HOME", "/tmp/pvm-test-xyz")
	p, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if p.Home != "/tmp/pvm-test-xyz" {
		t.Fatalf("home %s", p.Home)
	}
	if got := p.AgentSock("dev1"); got != "/tmp/pvm-test-xyz/vms/dev1/agent.sock" {
		t.Fatalf("agent sock %s", got)
	}
	if got := p.AgentBin(); got != filepath.Join(p.Runtime, native.GuestBinary("agent")) {
		t.Fatalf("agent bin %s", got)
	}
}

func TestCheckPermissionsDoesNotModifyExistingRoot(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "state"))
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := p.CheckPermissions(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p.Home, 0755); err != nil {
		t.Fatal(err)
	}
	if err := p.CheckPermissions(); err == nil {
		t.Fatal("exposed state root not detected")
	}
	info, err := os.Stat(p.Home)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Fatal("diagnostic modified user directory")
	}
}

func TestEnsurePreservesExistingSharePermissions(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "state"))
	if err := os.MkdirAll(p.Share, 0700); err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p.Share)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatal("existing share permissions changed")
	}
}

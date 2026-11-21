package paths

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
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

// A stored port that can no longer be bound, as when Windows reserves a
// range covering it after a reboot, is replaced; a usable one is kept.
func TestRefreshEndpoint(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "qmp.sock")
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	busy := "tcp:" + held.Addr().String()
	if err := os.WriteFile(sock+".tcp", []byte(busy+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := refreshEndpoint(sock)
	if err != nil || !changed {
		t.Fatalf("unusable port kept (changed=%v, err=%v)", changed, err)
	}
	fresh := storedEndpoint(sock)
	if fresh == "" || fresh == busy {
		t.Fatalf("new endpoint %q, was %q", fresh, busy)
	}
	changed, err = refreshEndpoint(sock)
	if err != nil || changed {
		t.Fatalf("usable port replaced (changed=%v, err=%v)", changed, err)
	}
	if got := storedEndpoint(sock); got != fresh {
		t.Fatalf("endpoint changed to %q, want %q", got, fresh)
	}
	changed, err = refreshEndpoint(filepath.Join(t.TempDir(), "none.sock"))
	if err != nil || changed {
		t.Fatalf("refreshed an endpoint that was never stored (changed=%v, err=%v)", changed, err)
	}
}

func TestRefreshEndpointWriteFailure(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("requires Unix permission enforcement and a non-root user")
	}
	dir := t.TempDir()
	sock := filepath.Join(dir, "qmp.sock")
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := os.WriteFile(sock+".tcp", []byte("tcp:"+held.Addr().String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	changed, err := refreshEndpoint(sock)
	if err == nil || changed {
		t.Fatalf("write failure hidden (changed=%v, err=%v)", changed, err)
	}
}

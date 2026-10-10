package paths

import (
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

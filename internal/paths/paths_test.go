package paths

import (
	"path/filepath"
	"testing"
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
	if got := p.AgentBin("arm64"); got != filepath.Join(p.Runtime, "agent-linux-arm64") {
		t.Fatalf("agent bin %s", got)
	}
}

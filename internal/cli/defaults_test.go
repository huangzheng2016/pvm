package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huangzheng2016/pvm/internal/paths"
	"github.com/huangzheng2016/pvm/internal/vm"
)

func TestResolveDefaultVM(t *testing.T) {
	p := paths.New(t.TempDir())
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveVMName(p, "", false); err == nil {
		t.Fatal("empty state should have no default VM")
	}
	for _, name := range []string{"dev1", "dev2"} {
		if err := os.MkdirAll(p.VMDir(name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := (&vm.Spec{Name: name, User: "dev", Role: "user"}).Save(p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := resolveVMName(p, "", false); err == nil || !strings.Contains(err.Error(), "multiple VMs") {
		t.Fatalf("ambiguous VMs must error: %v", err)
	}
	if _, err := resolveVMName(p, "", true); err == nil {
		t.Fatal("destructive command must never guess a VM")
	}
	if err := saveDefaults(p, defaultsState{DefaultVM: "dev2"}); err != nil {
		t.Fatal(err)
	}
	got, rest, err := resolveVMNameFromArgs(p, []string{"whoami"})
	if err != nil || got != "dev2" || len(rest) != 1 || rest[0] != "whoami" {
		t.Fatalf("default VM command args = %q, %v, %v", got, rest, err)
	}
	got, rest, err = resolveVMNameFromArgs(p, []string{"dev1", "echo"})
	if err != nil || got != "dev1" || len(rest) != 1 {
		t.Fatalf("explicit VM = %q, %v, %v", got, rest, err)
	}
	ClearDefaultVMIfRemoved(p, "dev2")
	if _, err := os.Stat(filepath.Join(p.Home, "defaults.json")); err != nil {
		t.Fatal(err)
	}
	if st := loadDefaults(p); st.DefaultVM != "" {
		t.Fatalf("removed default VM not cleared: %+v", st)
	}
}

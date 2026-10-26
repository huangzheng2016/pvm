package cluster

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/huangzheng2016/pvm/internal/gw/rules"
)

func TestMutationsRollBackOnSaveFailure(t *testing.T) {
	tests := map[string]func(*Store) error{
		"ensure":     func(s *Store) error { _, err := s.Ensure(2); return err },
		"add-vm":     func(s *Store) error { _, err := s.AddVM(1, "dev2"); return err },
		"remove-vm":  func(s *Store) error { return s.RemoveVM(1, "dev1") },
		"add-map":    func(s *Store) error { _, err := s.AddMap(1, "dev1", "100.99.1.10", 18081, 81, "tcp"); return err },
		"remove-map": func(s *Store) error { _, err := s.RemoveMap(1, 18080, "tcp"); return err },
		"set-maps":   func(s *Store) error { return s.SetMaps(1, nil) },
		"add-rule": func(s *Store) error {
			_, err := s.AddRule(1, rules.Rule{Type: rules.DomainSuffix, Pattern: "example.org", Action: rules.Direct})
			return err
		},
		"replace-rule": func(s *Store) error {
			_, err := s.AddRule(1, rules.Rule{Type: rules.DomainSuffix, Pattern: "example.com", Action: rules.Direct})
			return err
		},
		"remove-rule":  func(s *Store) error { return s.RemoveRule(1, 0) },
		"set-rules":    func(s *Store) error { return s.SetRules(1, nil) },
		"vm-exit":      func(s *Store) error { return s.SetVMExit(1, "dev1", "direct") },
		"default-exit": func(s *Store) error { return s.SetDefaultExit(1, "direct") },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s, err := LoadStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Ensure(1); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AddVM(1, "dev1"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AddMap(1, "dev1", "100.99.1.10", 18080, 80, "tcp"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AddRule(1, rules.Rule{Type: rules.DomainSuffix, Pattern: "example.com", Action: rules.Proxy, Exit: "my-exit"}); err != nil {
				t.Fatal(err)
			}
			if err := s.SetDefaultExit(1, "my-exit"); err != nil {
				t.Fatal(err)
			}
			if err := s.SetVMExit(1, "dev1", "my-exit"); err != nil {
				t.Fatal(err)
			}
			before := s.List()
			oldPath := s.path
			blocker := filepath.Join(dir, "blocker")
			if err := os.WriteFile(blocker, nil, 0600); err != nil {
				t.Fatal(err)
			}
			s.path = filepath.Join(blocker, "clusters.json")
			if err := mutate(s); err == nil {
				t.Fatal("write failure was hidden")
			}
			if got := s.List(); !reflect.DeepEqual(got, before) {
				t.Fatalf("failed mutation changed in-memory state: %#v", got)
			}
			reloaded, err := LoadStore(filepath.Dir(oldPath))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(reloaded.List(), before) {
				t.Fatal("failed mutation changed persisted state")
			}
		})
	}
}

func TestAddVMExistingReturnsCopy(t *testing.T) {
	s, err := LoadStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ensure(1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddVM(1, "dev1"); err != nil {
		t.Fatal(err)
	}
	v, err := s.AddVM(1, "dev1")
	if err != nil {
		t.Fatal(err)
	}
	v.IP = "203.0.113.1"
	if s.Get(1).VMs["dev1"].IP == v.IP {
		t.Fatal("returned VM aliases store state")
	}
}

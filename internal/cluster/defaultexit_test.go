package cluster

import (
	"testing"

	"github.com/huangzheng2016/pvm/internal/gw/rules"
)

func TestSetDefaultExit(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ensure(1); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDefaultExit(1, "my-exit"); err != nil {
		t.Fatal(err)
	}
	got := s.Get(1)
	if got.DefaultExit != "my-exit" {
		t.Fatalf("default exit = %q", got.DefaultExit)
	}
	got.DefaultExit = "mutated"
	if s.Get(1).DefaultExit != "my-exit" {
		t.Fatal("store mutated through a returned copy")
	}
	s2, err := LoadStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Get(1).DefaultExit != "my-exit" {
		t.Fatal("default exit not persisted")
	}
	if err := s2.SetDefaultExit(1, "direct"); err != nil {
		t.Fatal(err)
	}
	if s2.Get(1).DefaultExit != "" {
		t.Fatal("direct must clear the default exit")
	}
	if err := s2.SetDefaultExit(9, "my-exit"); err == nil {
		t.Fatal("unknown cluster must error")
	}
}

func TestSetRulesRestoresSnapshot(t *testing.T) {
	s, err := LoadStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ensure(1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddRule(1, rules.Rule{Type: rules.DomainSuffix, Pattern: "example.com", Action: rules.Direct}); err != nil {
		t.Fatal(err)
	}
	before := s.Get(1).Rules
	if _, err := s.AddRule(1, rules.Rule{Type: rules.DomainSuffix, Pattern: "example.com", Action: rules.Proxy, Exit: "my-exit"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRules(1, before); err != nil {
		t.Fatal(err)
	}
	got := s.Get(1).Rules
	if len(got) != 1 || got[0].Action != rules.Direct || got[0].Exit != "" {
		t.Fatalf("restored rules = %+v, want direct rule", got)
	}
}

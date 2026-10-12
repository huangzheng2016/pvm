package cluster

import "testing"

func TestSetDefaultExit(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ensure(1); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDefaultExit(1, "us"); err != nil {
		t.Fatal(err)
	}
	got := s.Get(1)
	if got.DefaultExit != "us" {
		t.Fatalf("default exit = %q", got.DefaultExit)
	}
	got.DefaultExit = "mutated"
	if s.Get(1).DefaultExit != "us" {
		t.Fatal("store mutated through a returned copy")
	}
	s2, err := LoadStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Get(1).DefaultExit != "us" {
		t.Fatal("default exit not persisted")
	}
	if err := s2.SetDefaultExit(1, "direct"); err != nil {
		t.Fatal(err)
	}
	if s2.Get(1).DefaultExit != "" {
		t.Fatal("direct must clear the default exit")
	}
	if err := s2.SetDefaultExit(9, "us"); err == nil {
		t.Fatal("unknown cluster must error")
	}
}

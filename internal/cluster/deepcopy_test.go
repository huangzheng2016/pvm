package cluster

import "testing"

func TestGetReturnsDeepCopy(t *testing.T) {
	s, err := LoadStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cl, err := s.Ensure(1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddVM(1, "dev1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVMExit(1, "dev1", "my-exit"); err != nil {
		t.Fatal(err)
	}
	got := s.Get(1)
	if got == nil {
		t.Fatal("Get returned nil")
	}
	got.VMs["hacked"] = &VMNet{Name: "hacked"}
	got.Maps = append(got.Maps, &PortMap{VM: "hacked"})
	got.VMExits["hacked"] = "my-exit"
	delete(got.VMs, "dev1")

	again := s.Get(1)
	if again.VMs["dev1"] == nil {
		t.Fatal("store VM map was mutated through a returned copy")
	}
	if again.VMs["hacked"] != nil || len(again.Maps) != 0 || again.VMExits["hacked"] != "" {
		t.Fatal("store slices/maps were mutated through a returned copy")
	}
	// And the VMNet pointer itself must be independent.
	again.VMs["dev1"].IP = "1.2.3.4"
	if s.Get(1).VMs["dev1"].IP == "1.2.3.4" {
		t.Fatal("nested VMNet was mutated through a returned copy")
	}
	_ = cl
}

func TestRemoveVMClearsVMExits(t *testing.T) {
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
	if err := s.SetVMExit(1, "dev1", "my-exit"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveVM(1, "dev1"); err != nil {
		t.Fatal(err)
	}
	if got := s.Get(1); len(got.VMExits) != 0 {
		t.Fatalf("VMExits leaked after RemoveVM: %+v", got.VMExits)
	}
}

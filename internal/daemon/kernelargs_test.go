package daemon

import (
	"testing"

	"github.com/huangzheng2016/pvm/internal/vm"
)

func TestClearKernelArgsPendingPersists(t *testing.T) {
	spec := &vm.Spec{Name: "dev1", User: "u", KernelArgs: "noxsave", KernelArgsPending: true}
	s := newAuditTestServer(t, spec)
	if err := s.clearKernelArgsPending(spec); err != nil {
		t.Fatal(err)
	}
	saved, err := vm.Load(s.P, "dev1")
	if err != nil {
		t.Fatal(err)
	}
	if saved.KernelArgsPending || spec.KernelArgsPending {
		t.Error("pending flag not cleared")
	}
	if cached, err := s.getSpec("dev1"); err != nil || cached.KernelArgsPending {
		t.Errorf("cached spec still pending: %+v, %v", cached, err)
	}
}

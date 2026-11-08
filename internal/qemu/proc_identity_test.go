package qemu

import (
	"os"
	"testing"
)

func TestIdentityOfCurrentAndMissingProcess(t *testing.T) {
	id, err := IdentityOf(os.Getpid())
	if err != nil {
		t.Fatalf("IdentityOf(self): %v", err)
	}
	if !id.Alive() {
		t.Fatal("self reported not alive")
	}
	missing, err := IdentityOf(999999)
	if err != errNoProc {
		t.Fatalf("missing process error = %v, want errNoProc", err)
	}
	if missing != nil && missing.Alive() {
		t.Fatal("missing process reported alive")
	}
}

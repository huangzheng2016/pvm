package keys

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureRecoversMissingPublicKeyWithoutRotatingPrivateKey(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "id_ed25519")
	if _, err := Ensure(keyPath); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keyPath + ".pub"); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(keyPath); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("private key changed while recovering missing public key")
	}
	if _, err := os.Stat(keyPath + ".pub"); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureRejectsMismatchedKeypair(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	if _, err := Ensure(keyPath); err != nil {
		t.Fatal(err)
	}
	otherPath := filepath.Join(dir, "other")
	otherPub, err := Ensure(otherPath)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath+".pub", []byte(otherPub), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(keyPath); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected mismatched keypair error, got %v", err)
	}
	after, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("private key changed after mismatched keypair")
	}
}

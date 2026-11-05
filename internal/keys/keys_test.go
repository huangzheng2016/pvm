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

func TestEnsurePreservesOrphanPublicKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "id_ed25519")
	const public = "existing-public-key"
	if err := os.WriteFile(path+".pub", []byte(public), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(path); err == nil {
		t.Fatal("missing private key silently replaced")
	}
	got, err := os.ReadFile(path + ".pub")
	if err != nil || string(got) != public {
		t.Fatal("public key overwritten")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("replacement private key created")
	}
}

func TestEnsureRejectsExposedPrivateKeyWithoutChangingIt(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "id_ed25519")
	if _, err := Ensure(keyPath); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(keyPath, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(keyPath); err == nil {
		t.Fatal("exposed private key accepted")
	}
	after, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || info.Mode().Perm() != 0644 {
		t.Fatal("existing key was silently changed")
	}
}

func TestEnsureRejectsPrivateKeySymlink(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "original")
	if _, err := Ensure(original); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "linked")
	if err := os.Symlink(original, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(link); err == nil {
		t.Fatal("symlinked key accepted")
	}
}

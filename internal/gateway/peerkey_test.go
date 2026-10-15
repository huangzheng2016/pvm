package gateway

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/huangzheng2016/pvm/internal/tunnel"
)

func TestLoadOrCreatePeerKey(t *testing.T) {
	dir := t.TempDir()
	first, err := loadOrCreatePeerKey(dir, "my-exit")
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadOrCreatePeerKey(dir, "my-exit")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("saved peer key was not reused")
	}
	if _, err := tunnel.PublicKey(first); err != nil {
		t.Fatalf("saved key is invalid: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "my-exit.key"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode = %o", info.Mode().Perm())
	}
	if _, err := loadOrCreatePeerKey(dir, "../bad"); err == nil {
		t.Fatal("invalid exit name must fail")
	}
}

func TestLoadOrCreatePeerKeyRejectsCorruptSavedKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "my-exit.key")
	if err := os.WriteFile(path, []byte("broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreatePeerKey(dir, "my-exit"); err == nil {
		t.Fatal("corrupt saved key must fail instead of being replaced")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "broken\n" {
		t.Fatal("corrupt saved key was overwritten")
	}
}

func TestSameExitPeer(t *testing.T) {
	a := ExitDef{Server: "exit.example:18443", Token: "token", Transport: "tcp", Fingerprint: "fp"}
	b := a
	if !sameExitPeer(a, b) {
		t.Fatal("identical definitions must match")
	}
	b.Server = "other.example:18443"
	if sameExitPeer(a, b) {
		t.Fatal("different servers must not match")
	}
}

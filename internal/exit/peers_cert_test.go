package exit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPeerStoreAllocateAndPersist(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadPeerStore(filepath.Join(dir, "peers.json"))
	if err != nil {
		t.Fatal(err)
	}
	p1, _, err := s.Allocate("key-a", "host-a", "udp")
	if err != nil {
		t.Fatal(err)
	}
	if p1.IP != "10.99.0.2" {
		t.Fatalf("first udp peer = %s, want 10.99.0.2", p1.IP)
	}
	p2, _, err := s.Allocate("key-b", "host-b", "udp")
	if err != nil {
		t.Fatal(err)
	}
	if p2.IP != "10.99.0.3" {
		t.Fatalf("second udp peer = %s, want 10.99.0.3", p2.IP)
	}
	// tcp pool is a separate subnet
	p3, _, err := s.Allocate("key-a", "host-a", "tcp")
	if err != nil {
		t.Fatal(err)
	}
	if p3.IP != "10.100.0.2" {
		t.Fatalf("first tcp peer = %s, want 10.100.0.2", p3.IP)
	}
	again, _, err := s.Allocate("key-a", "host-a", "udp")
	if err != nil {
		t.Fatal(err)
	}
	if again.IP != p1.IP {
		t.Fatalf("re-enroll got %s, want %s", again.IP, p1.IP)
	}
	if len(s.List()) != 3 {
		t.Fatalf("peers = %d, want 3", len(s.List()))
	}

	// reload from disk
	s2, err := LoadPeerStore(filepath.Join(dir, "peers.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.List()) != 3 {
		t.Fatalf("reloaded peers = %d, want 3", len(s2.List()))
	}

	// remove frees the slot for reuse
	if _, ok, err := s2.Remove("10.99.0.3"); err != nil || !ok {
		t.Fatalf("remove: ok=%v err=%v", ok, err)
	}
	p4, _, err := s2.Allocate("key-c", "host-c", "udp")
	if err != nil {
		t.Fatal(err)
	}
	if p4.IP != "10.99.0.3" {
		t.Fatalf("after remove, peer = %s, want 10.99.0.3", p4.IP)
	}
	if _, ok, _ := s2.Remove("10.99.0.99"); ok {
		t.Fatal("remove of unknown ip should report ok=false")
	}

	// file permissions
	fi, err := os.Stat(filepath.Join(dir, "peers.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("peers.json perm = %o, want 600", fi.Mode().Perm())
	}
}

func TestPeerStoreRejectsBadTransport(t *testing.T) {
	s, err := LoadPeerStore(filepath.Join(t.TempDir(), "peers.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Allocate("k", "h", "gre"); err == nil || !strings.Contains(err.Error(), "unknown transport") {
		t.Fatalf("want unknown transport error, got %v", err)
	}
}

func TestEnsureCertStableAndPinned(t *testing.T) {
	dir := t.TempDir()
	c1, fp1, err := EnsureCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fp1 == "" || len(c1.Certificate) == 0 {
		t.Fatal("empty cert or fingerprint")
	}
	_, fp2, err := EnsureCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fp1 != fp2 {
		t.Fatal("fingerprint changed across reload")
	}
	if _, err := Fingerprint(c1.Certificate[0]); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "server-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("key perm = %o, want 600", fi.Mode().Perm())
	}
}

func TestServerIPPerTransport(t *testing.T) {
	udp, err := ServerIP("udp")
	if err != nil || udp != "10.99.0.1" {
		t.Fatalf("udp server ip = %q, %v", udp, err)
	}
	tcp, err := ServerIP("tcp")
	if err != nil || tcp != "10.100.0.1" {
		t.Fatalf("tcp server ip = %q, %v", tcp, err)
	}
}

package daemon

import (
	"strings"
	"testing"
)

func TestDesktopURLPublic(t *testing.T) {
	private := &desktopState{Password: "secret", HostPort: 18080}
	if got := desktopURL(private, "dev1"); !strings.HasPrefix(got, "http://127.0.0.1:18080/") {
		t.Fatalf("private URL = %q", got)
	}
	public := &desktopState{Password: "secret", HostPort: 18080, Public: true}
	if got := desktopURL(public, "dev1"); !strings.HasPrefix(got, "http://0.0.0.0:18080/") {
		t.Fatalf("public URL = %q", got)
	}
}

func TestEnsurePublicRelay(t *testing.T) {
	port, err := freeLoopbackPort("tcp")
	if err != nil {
		t.Fatal(err)
	}
	poolPort := 18081
	r := newRelaySet()
	key := relayKey("tcp", port)
	if err := r.Ensure("tcp", port, poolPort); err != nil {
		t.Fatal(err)
	}
	if got := r.bind[key]; got != "127.0.0.1" {
		t.Fatalf("default bind = %q", got)
	}
	if err := r.EnsurePublic("tcp", port, poolPort); err == nil {
		t.Fatal("changing bind without removing the old relay must fail")
	}
	r.Remove("tcp", port)
	if err := r.EnsurePublic("tcp", port, poolPort); err != nil {
		t.Fatal(err)
	}
	defer r.Remove("tcp", port)
	if got := r.bind[key]; got != "0.0.0.0" {
		t.Fatalf("public bind = %q", got)
	}
	if got := r.tcp[key].Addr().String(); strings.HasPrefix(got, "127.0.0.1:") {
		t.Fatalf("public listener is loopback-only: %q", got)
	}
}

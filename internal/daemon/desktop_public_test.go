package daemon

import (
	"net"
	"strings"
	"testing"

	"github.com/huangzheng2016/pvm/internal/vm"
)

func freeRelayTestPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

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
	port := freeRelayTestPort(t)
	poolPort := freeRelayTestPort(t)
	r := newRelaySet()
	key := relayKey("tcp", port)
	if err := r.Ensure("tcp", port, poolPort); err != nil {
		t.Fatal(err)
	}
	if bind, ok := r.Binding("tcp", port); !ok || bind != "127.0.0.1" {
		t.Fatalf("default binding = %q, %v", bind, ok)
	}
	if err := r.EnsurePublic("tcp", port, poolPort); err == nil {
		t.Fatal("changing bind without removing the old relay must fail")
	}
	r.Remove("tcp", port)
	if err := r.EnsurePublic("tcp", port, poolPort); err != nil {
		t.Fatal(err)
	}
	defer r.Remove("tcp", port)
	if bind, ok := r.Binding("tcp", port); !ok || bind != "0.0.0.0" {
		t.Fatalf("public binding = %q, %v", bind, ok)
	}
	if got := r.tcp[key].Addr().String(); strings.HasPrefix(got, "127.0.0.1:") {
		t.Fatalf("public listener is loopback-only: %q", got)
	}
}

func TestDesktopRelayMatches(t *testing.T) {
	s := newPushTestServer(t)
	s.relays = newRelaySet()
	if _, err := s.Clusters.Ensure(1); err != nil {
		t.Fatal(err)
	}
	hostPort := freeRelayTestPort(t)
	m, err := s.Clusters.AddMap(1, "dev1", "100.99.1.12", hostPort, desktopWebPort, "tcp")
	if err != nil {
		t.Fatal(err)
	}
	spec := &vm.Spec{Name: "dev1", ClusterID: 1}
	st := &desktopState{HostPort: hostPort}
	if err := s.relays.Ensure("tcp", m.HostPort, m.PoolPort); err != nil {
		t.Fatal(err)
	}
	if !s.desktopRelayMatches(spec, st, false) {
		t.Fatal("private relay must match private mode")
	}
	if s.desktopRelayMatches(spec, st, true) {
		t.Fatal("private relay must not match public mode")
	}
	s.relays.Remove("tcp", m.HostPort)
	if err := s.relays.EnsurePublic("tcp", m.HostPort, m.PoolPort); err != nil {
		t.Fatal(err)
	}
	defer s.relays.Remove("tcp", m.HostPort)
	if !s.desktopRelayMatches(spec, st, true) {
		t.Fatal("public relay must match public mode")
	}
}

package daemon

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"strings"
	"testing"
)

func freeRelayTestPort(t *testing.T) int {
	t.Helper()
	addr := "0.0.0.0:0"
	if runtime.GOOS == "windows" {
		// see skipPublicBindOnWindows; tests needing 0.0.0.0 skip there
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// skipPublicBindOnWindows skips tests that listen on 0.0.0.0: Windows
// asks the user to allow every new test binary through the firewall.
func skipPublicBindOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("listening on 0.0.0.0 prompts for a firewall exception on Windows")
	}
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
	skipPublicBindOnWindows(t)
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

// Switching to or from public mode rebinds the listener on the same port,
// so the URL keeps its port.
func TestDesktopForwardRebindsOnPublicChange(t *testing.T) {
	skipPublicBindOnWindows(t)
	s := &Server{}
	port, err := s.ensureDesktopForward("dev1", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.stopDesktopForward("dev1")
	if got := s.desktopForwards["dev1"].bind; got != "127.0.0.1" {
		t.Fatalf("private bind = %q", got)
	}
	if again, err := s.ensureDesktopForward("dev1", 0, false); err != nil || again != port {
		t.Fatalf("same mode: port %d, %v; want %d", again, err, port)
	}
	pub, err := s.ensureDesktopForward("dev1", port, true)
	if err != nil || pub != port {
		t.Fatalf("public: port %d, %v; want %d", pub, err, port)
	}
	if got := s.desktopForwards["dev1"].bind; got != "0.0.0.0" {
		t.Fatalf("public bind = %q", got)
	}
	s.stopDesktopForward("dev1")
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("port still held after stop: %v", err)
	}
	ln.Close()
}

// A desktop map from before pvm.stream is removed with its relay; other
// maps of the VM stay.
func TestRemoveDesktopMapsDropsLegacyMap(t *testing.T) {
	s, dev1, _ := newMapTestServer(t)
	legacy := freeRelayTestPort(t)
	other := freeRelayTestPort(t)
	for _, m := range []struct{ host, guest int }{{legacy, desktopWebPort}, {other, 8080}} {
		pm, err := s.Clusters.AddMap(dev1.ClusterID, dev1.Name, dev1.IP, m.host, m.guest, "tcp")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.relays.Ensure("tcp", pm.HostPort, pm.PoolPort); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.removeDesktopMaps(context.Background(), dev1, okPush); err != nil {
		t.Fatal(err)
	}
	maps := s.Clusters.Get(dev1.ClusterID).Maps
	if len(maps) != 1 || maps[0].GuestPort != 8080 {
		t.Fatalf("maps after cleanup: %+v", maps)
	}
	if _, ok := s.relays.Binding("tcp", legacy); ok {
		t.Fatal("legacy desktop relay still bound")
	}
	if _, ok := s.relays.Binding("tcp", other); !ok {
		t.Fatal("unrelated relay removed")
	}
}

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/huangzheng2016/pvm/internal/agent"
	"github.com/huangzheng2016/pvm/internal/cluster"
	"github.com/huangzheng2016/pvm/internal/paths"
	"github.com/huangzheng2016/pvm/internal/vm"
)

// newMapTestServer builds a Server with a temp cluster store, working
// relay set, and dev1/dev2 saved as cluster VMs. PVM_HOME stays short:
// unix socket paths (agent.sock) must fit sun_path.
func newMapTestServer(t *testing.T) (*Server, *vm.Spec, *vm.Spec) {
	t.Helper()
	home, err := os.MkdirTemp("", "pvm-map-")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PVM_HOME", home)
	t.Cleanup(func() { os.RemoveAll(home) })
	p, err := paths.Default()
	if err != nil {
		t.Fatal(err)
	}
	st, err := cluster.LoadStore(p.Home + "/clusters")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{P: p, Clusters: st, relays: newRelaySet(), vms: map[string]*vm.Spec{}, agents: map[string]*agent.Client{}}
	t.Cleanup(func() {
		s.relays.mu.Lock()
		defer s.relays.mu.Unlock()
		for k, rel := range s.relays.tcp {
			rel.Close()
			delete(s.relays.tcp, k)
		}
		for k, rel := range s.relays.udp {
			rel.close()
			delete(s.relays.udp, k)
		}
	})
	cl, err := st.Ensure(cluster.DefaultClusterID)
	if err != nil {
		t.Fatal(err)
	}
	specs := make([]*vm.Spec, 0, 2)
	for _, name := range []string{"dev1", "dev2"} {
		vnet, err := st.AddVM(cl.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		spec := &vm.Spec{Name: name, Role: "user", User: "pvm", ClusterID: cl.ID, IP: vnet.IP, MAC: vnet.MAC}
		if err := os.MkdirAll(p.VMDir(name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := spec.Save(p); err != nil {
			t.Fatal(err)
		}
		s.vms[name] = spec
		specs = append(specs, spec)
	}
	return s, specs[0], specs[1]
}

func okPush(ctx context.Context, clusterID int) error { return nil }

// testPorts remembers ports already handed out in this test process:
// freeLoopbackPort's check-then-close can otherwise return the same
// port twice in a row, and a plain retry loop would spin on it.
var testPorts sync.Map // int -> bool

// freePort returns a currently-free loopback port never before handed
// out in this process, so parallel test runs never collide.
func freePort(t *testing.T, proto string) int {
	t.Helper()
	for p := 18080; p < 18080+2000; p++ {
		if _, taken := testPorts.LoadOrStore(p, true); taken {
			continue
		}
		if portFree(proto, p) == nil {
			return p
		}
	}
	t.Fatal("no free test port")
	return 0
}

func TestHandleMapAddValidation(t *testing.T) {
	s, _, _ := newMapTestServer(t)
	cases := []struct {
		name string
		body string
		code int
	}{
		{"missing vm", `{"guest_port":80}`, 400},
		{"missing guest port", `{"vm":"dev1"}`, 400},
		{"guest port out of range", `{"vm":"dev1","guest_port":70000}`, 400},
		{"unknown vm", `{"vm":"ghost","guest_port":80}`, 404},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/maps", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			s.handleMapAdd(rec, req)
			if rec.Code != tc.code {
				t.Fatalf("code = %d, want %d (%s)", rec.Code, tc.code, rec.Body)
			}
		})
	}
}

func TestAddMapSuccess(t *testing.T) {
	s, dev1, _ := newMapTestServer(t)
	port := freePort(t, "tcp")
	pushed := 0
	push := func(ctx context.Context, clusterID int) error {
		pushed++
		if clusterID != dev1.ClusterID {
			t.Errorf("push cluster = %d", clusterID)
		}
		return nil
	}
	m, code, err := s.addMap(context.Background(), dev1, port, 80, "tcp", push)
	if err != nil || code != 201 {
		t.Fatalf("addMap = %v, %d, %v", m, code, err)
	}
	if m.VM != "dev1" || m.HostPort != port || m.GuestPort != 80 || m.Proto != "tcp" || m.PoolPort == 0 {
		t.Fatalf("bad map %+v", m)
	}
	if pushed != 1 {
		t.Fatalf("push calls = %d", pushed)
	}
	if err := portFree("tcp", port); err == nil {
		t.Fatal("relay must hold the host port after add")
	}
}

func TestAddMapAutoHostPort(t *testing.T) {
	s, dev1, _ := newMapTestServer(t)
	m, _, err := s.addMap(context.Background(), dev1, 0, 80, "tcp", okPush)
	if err != nil {
		t.Fatal(err)
	}
	if m.HostPort < 18080 || m.HostPort >= 18080+2000 {
		t.Fatalf("auto host port %d out of pool", m.HostPort)
	}
}

func TestAddMapRollbackOnPushFailure(t *testing.T) {
	s, dev1, _ := newMapTestServer(t)
	port := freePort(t, "tcp")
	push := func(ctx context.Context, clusterID int) error { return errors.New("gateway away") }
	m, code, err := s.addMap(context.Background(), dev1, port, 80, "tcp", push)
	if err == nil || code != 500 || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("want rolled-back 500, got %v %d %v", m, code, err)
	}
	if got := s.Clusters.Get(dev1.ClusterID).Maps; len(got) != 0 {
		t.Fatalf("map must be rolled back, store has %+v", got)
	}
	if err := portFree("tcp", port); err != nil {
		t.Fatal("relay must be removed by rollback")
	}
}

func TestAddMapDuplicateHostPort(t *testing.T) {
	s, dev1, dev2 := newMapTestServer(t)
	port := freePort(t, "tcp")
	if _, _, err := s.addMap(context.Background(), dev1, port, 80, "tcp", okPush); err != nil {
		t.Fatal(err)
	}
	_, code, err := s.addMap(context.Background(), dev2, port, 80, "tcp", okPush)
	if err == nil || code != 409 {
		t.Fatalf("duplicate add = %d %v, want 409", code, err)
	}
	// Same port on the other proto is a different mapping and allowed.
	if _, _, err := s.addMap(context.Background(), dev2, port, 80, "udp", okPush); err != nil {
		t.Fatalf("udp on same port: %v", err)
	}
}

func TestAddMapRejectsNonClusterVM(t *testing.T) {
	s, _, _ := newMapTestServer(t)
	port := freePort(t, "tcp")
	gw := &vm.Spec{Name: "gw1", Role: "gateway"}
	if _, code, err := s.addMap(context.Background(), gw, port, 80, "tcp", okPush); err == nil || code != 400 {
		t.Fatalf("gateway add = %d %v, want 400", code, err)
	}
	solo := &vm.Spec{Name: "solo", Role: "user"}
	if _, code, err := s.addMap(context.Background(), solo, port, 80, "tcp", okPush); err == nil || code != 400 {
		t.Fatalf("standalone add = %d %v, want 400", code, err)
	}
}

func TestHandleMapRemoveUnchanged(t *testing.T) {
	// The global host API keeps its remove-by-port semantics.
	s, dev1, _ := newMapTestServer(t)
	port := freePort(t, "tcp")
	if _, _, err := s.addMap(context.Background(), dev1, port, 80, "tcp", okPush); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("DELETE", "/v1/maps/18084/tcp", nil)
	req.SetPathValue("port", fmt.Sprint(port))
	req.SetPathValue("proto", "tcp")
	rec := httptest.NewRecorder()
	// pushGatewayConfig fails without a gateway VM, so removal must roll
	// back and report the failure rather than silently dropping the map.
	s.handleMapRemove(rec, req)
	if rec.Code != 500 {
		t.Fatalf("remove without gateway = %d %s, want rolled-back 500", rec.Code, rec.Body)
	}
	if got := s.Clusters.Get(dev1.ClusterID).Maps; len(got) != 1 {
		t.Fatalf("failed removal must restore the map, store has %+v", got)
	}
}

func TestMapEntryJSONShape(t *testing.T) {
	// The guest wire shape must stay decoupled from cluster.PortMap.
	data, err := json.Marshal(mapEntry(&cluster.PortMap{VM: "dev1", VMIP: "192.168.1.2", HostPort: 8080, PoolPort: 15001, GuestPort: 80, Proto: "tcp"}))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 3 || fields["host_port"] != 8080.0 || fields["guest_port"] != 80.0 || fields["proto"] != "tcp" {
		t.Fatalf("guest map entry leaks host fields: %s", data)
	}
}

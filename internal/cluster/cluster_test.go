package cluster

import (
	"path/filepath"
	"testing"
)

func TestEnsureAndIPAM(t *testing.T) {
	s, err := LoadStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c1, err := s.Ensure(1)
	if err != nil {
		t.Fatal(err)
	}
	if c1.CIDR != "100.99.1.0/24" || c1.GwIP != "100.99.1.1" || c1.Gateway != "pvm-gw1" {
		t.Errorf("cluster fields: %+v", c1)
	}
	// PoolBase depends on host port availability (another pvm gateway may
	// hold the preferred window), so assert the window shape only.
	if c1.PoolBase < 30000 || c1.PoolBase%100 != 0 || c1.PoolSize != 100 {
		t.Errorf("pool: %+v", c1)
	}
	v1, _ := s.AddVM(1, "dev1")
	v2, _ := s.AddVM(1, "dev2")
	if v1.IP != "100.99.1.10" || v2.IP != "100.99.1.11" {
		t.Errorf("ipam: %s %s", v1.IP, v2.IP)
	}
	if v1.MAC == v2.MAC || v1.MAC != "10:65:30:01:00:0a" {
		t.Errorf("macs: %s %s", v1.MAC, v2.MAC)
	}
	// idempotent
	v1b, _ := s.AddVM(1, "dev1")
	if v1b.IP != v1.IP {
		t.Errorf("re-add changed ip: %s", v1b.IP)
	}
	// removal frees the ip
	if err := s.RemoveVM(1, "dev1"); err != nil {
		t.Fatal(err)
	}
	v3, _ := s.AddVM(1, "dev3")
	if v3.IP != "100.99.1.10" {
		t.Errorf("ip not reused: %s", v3.IP)
	}
	// persistence
	s2, err := LoadStore(filepath.Dir(s.path))
	if err != nil {
		t.Fatal(err)
	}
	if s2.Get(1).VMs["dev2"].IP != "100.99.1.11" {
		t.Error("state not persisted")
	}
}

func TestMapPool(t *testing.T) {
	s, _ := LoadStore(t.TempDir())
	s.Ensure(1)
	s.AddVM(1, "dev1")
	m1, err := s.AddMap(1, "dev1", "100.99.1.10", 8080, 80, "tcp")
	if err != nil {
		t.Fatal(err)
	}
	c1, _ := s.Ensure(1)
	if m1.PoolPort != c1.PoolBase {
		t.Errorf("pool port: %d (base %d)", m1.PoolPort, c1.PoolBase)
	}
	m2, _ := s.AddMap(1, "dev1", "100.99.1.10", 8081, 81, "tcp")
	if m2.PoolPort != c1.PoolBase+1 {
		t.Errorf("pool port 2: %d", m2.PoolPort)
	}
	if _, err := s.AddMap(1, "dev1", "100.99.1.10", 8080, 90, "tcp"); err == nil {
		t.Error("duplicate host port accepted")
	}
	if err := s.RemoveMap(1, 8080, "tcp"); err != nil {
		t.Fatal(err)
	}
	m3, _ := s.AddMap(1, "dev1", "100.99.1.10", 8082, 82, "tcp")
	if m3.PoolPort != c1.PoolBase {
		t.Errorf("pool port not reused: %d", m3.PoolPort)
	}
	// udp gets its own pool port (separate dnat rule)
	m4, _ := s.AddMap(1, "dev1", "100.99.1.10", 8053, 53, "udp")
	if m4.Proto != "udp" {
		t.Errorf("proto: %s", m4.Proto)
	}
}

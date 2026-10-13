package exit

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// Allocation must walk the whole /16 host range, not stop at the last
// octet: with 10.99.0.2..10.99.0.255 taken, the next UDP peer lands at
// 10.99.1.0 — a host address the old per-octet scheme could never hand
// out (only the subnet's own network, server .0.1 and broadcast are
// skipped).
func TestAllocateCrossesSlash24Boundary(t *testing.T) {
	s, err := LoadPeerStore(filepath.Join(t.TempDir(), "peers.json"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= 255; i++ {
		s.Peers = append(s.Peers, Peer{
			IP:        fmt.Sprintf("10.99.0.%d", i),
			PubKey:    fmt.Sprintf("k%d", i),
			Transport: "udp",
		})
	}
	p, created, err := s.Allocate("k-new", "h", "udp")
	if err != nil || !created {
		t.Fatalf("allocate: created=%v err=%v", created, err)
	}
	if p.IP != "10.99.1.0" {
		t.Fatalf("peer beyond the old /24 = %s, want 10.99.1.0", p.IP)
	}
}

// The udp and tcp pools are distinct /16s: neither contains the other's
// addresses, so allocations from the two pools can never collide.
func TestTransportSubnetsDoNotOverlap(t *testing.T) {
	subnets, err := subnetsFor(defaultSubnetBase)
	if err != nil {
		t.Fatal(err)
	}
	if subnets["udp"] != "10.99.0.0/16" || subnets["tcp"] != "10.100.0.0/16" {
		t.Fatalf("default subnets = %v", subnets)
	}
	_, udpNet, _ := net.ParseCIDR(subnets["udp"])
	_, tcpNet, _ := net.ParseCIDR(subnets["tcp"])
	if udpNet.Contains(tcpNet.IP) || tcpNet.Contains(udpNet.IP) {
		t.Fatalf("subnets overlap: %v", subnets)
	}
	udpSrv, err := ServerIP("udp")
	if err != nil {
		t.Fatal(err)
	}
	tcpSrv, err := ServerIP("tcp")
	if err != nil {
		t.Fatal(err)
	}
	if udpNet.Contains(net.ParseIP(tcpSrv)) || tcpNet.Contains(net.ParseIP(udpSrv)) {
		t.Fatalf("server ip in foreign pool: udp=%s tcp=%s", udpSrv, tcpSrv)
	}

	s, err := LoadPeerStore(filepath.Join(t.TempDir(), "peers.json"))
	if err != nil {
		t.Fatal(err)
	}
	pu, _, err := s.Allocate("k", "h", "udp")
	if err != nil {
		t.Fatal(err)
	}
	pt, _, err := s.Allocate("k", "h", "tcp")
	if err != nil {
		t.Fatal(err)
	}
	if !udpNet.Contains(net.ParseIP(pu.IP)) || !tcpNet.Contains(net.ParseIP(pt.IP)) {
		t.Fatalf("peer outside its pool: udp=%s tcp=%s", pu.IP, pt.IP)
	}
}

func TestLoadPeerStoreRejectsBadBase(t *testing.T) {
	for _, base := range []int{-1, 0, 255, 1000} {
		if _, err := LoadPeerStoreWithBase(filepath.Join(t.TempDir(), "peers.json"), base); err == nil {
			t.Errorf("base %d must be rejected", base)
		}
	}
	for _, base := range []int{1, 99, 254} {
		if _, err := LoadPeerStoreWithBase(filepath.Join(t.TempDir(), "peers.json"), base); err != nil {
			t.Errorf("base %d must be accepted: %v", base, err)
		}
	}
}

func writePeersFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "peers.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const onePeerJSON = `{"ip":"10.99.0.2","pubkey":"k","hostname":"h","transport":"udp","created_at":"2026-01-01T00:00:00Z"}`

// A peers.json from the pre-layout (or a different-layout) generation is
// incompatible: its addresses belong to foreign subnets and would
// black-hole when replayed, so the load drops them and rewrites the file.
func TestStaleLayoutPeersDropped(t *testing.T) {
	for name, body := range map[string]string{
		"pre-layout":       `{"peers":[` + onePeerJSON + `]}`,
		"mismatch /24":     `{"subnets":{"udp":"10.99.0.0/24","tcp":"10.99.1.0/24"},"peers":[` + onePeerJSON + `]}`,
		"mismatch base 42": `{"subnets":{"udp":"10.42.0.0/16","tcp":"10.43.0.0/16"},"peers":[` + onePeerJSON + `]}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := writePeersFile(t, body)
			s, err := LoadPeerStore(path)
			if err != nil {
				t.Fatal(err)
			}
			if n := len(s.List()); n != 0 {
				t.Fatalf("stale peers kept: %d", n)
			}
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var disk PeerStore
			if err := json.Unmarshal(b, &disk); err != nil {
				t.Fatal(err)
			}
			if disk.Subnets["udp"] != "10.99.0.0/16" || disk.Subnets["tcp"] != "10.100.0.0/16" {
				t.Fatalf("rewritten layout = %v", disk.Subnets)
			}
			if len(disk.Peers) != 0 {
				t.Fatalf("rewritten peers = %v", disk.Peers)
			}
		})
	}
}

// A peers.json recorded under the current layout loads untouched.
func TestCurrentLayoutPeersKept(t *testing.T) {
	path := writePeersFile(t, `{"subnets":{"udp":"10.99.0.0/16","tcp":"10.100.0.0/16"},"peers":[`+onePeerJSON+`]}`)
	s, err := LoadPeerStore(path)
	if err != nil {
		t.Fatal(err)
	}
	peers := s.List()
	if len(peers) != 1 || peers[0].IP != "10.99.0.2" {
		t.Fatalf("peers = %v", peers)
	}
	// re-enroll reuses the persisted registration
	again, created, err := s.Allocate("k", "h", "udp")
	if err != nil {
		t.Fatal(err)
	}
	if created || again.IP != "10.99.0.2" {
		t.Fatalf("re-enroll = %s created=%v", again.IP, created)
	}
}

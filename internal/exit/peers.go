package exit

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/huangzheng2016/pvm/internal/fsutil"
)

type Peer struct {
	IP        string    `json:"ip"` // tunnel IP, e.g. 10.99.0.2
	PubKey    string    `json:"pubkey"`
	Hostname  string    `json:"hostname"`
	Transport string    `json:"transport"` // udp | tcp
	CreatedAt time.Time `json:"created_at"`
}

// PeerStore allocates tunnel IPs per transport subnet and persists to JSON.
// Subnets derive from a base octet: udp -> 10.<base>.0.0/16,
// tcp -> 10.<base+1>.0.0/16 (server holds .0.1). Default base 99.
// The layout is persisted alongside the peers; peers recorded under a
// different layout are dropped on load, because replaying them into the
// wireguard device would black-hole their (now foreign) addresses.
type PeerStore struct {
	mu      sync.Mutex
	path    string
	Subnets map[string]string `json:"subnets"`
	Peers   []Peer            `json:"peers"`
}

const defaultSubnetBase = 99

func subnetsFor(base int) (map[string]string, error) {
	if base < 1 || base > 254 {
		return nil, fmt.Errorf("subnet base %d out of range 1..254", base)
	}
	subnets := map[string]string{
		"udp": fmt.Sprintf("10.%d.0.0/16", base),
		"tcp": fmt.Sprintf("10.%d.0.0/16", base+1),
	}
	var nets []*net.IPNet
	for _, cidr := range subnets {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("bad subnet %q: %w", cidr, err)
		}
		for _, prev := range nets {
			if prev.Contains(n.IP) || n.Contains(prev.IP) {
				return nil, fmt.Errorf("subnets %q and %q overlap", prev.String(), cidr)
			}
		}
		nets = append(nets, n)
	}
	return subnets, nil
}

func SubnetFor(transport string) (string, error) {
	subnets, err := subnetsFor(defaultSubnetBase)
	if err != nil {
		return "", err
	}
	return subnetOf(subnets, transport)
}

func ServerIP(transport string) (string, error) {
	subnets, err := subnetsFor(defaultSubnetBase)
	if err != nil {
		return "", err
	}
	return serverIPOf(subnets, transport)
}

func subnetOf(subnets map[string]string, transport string) (string, error) {
	s, ok := subnets[transport]
	if !ok {
		return "", fmt.Errorf("unknown transport %q", transport)
	}
	return s, nil
}

func serverIPOf(subnets map[string]string, transport string) (string, error) {
	cidr, err := subnetOf(subnets, transport)
	if err != nil {
		return "", err
	}
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", err
	}
	ip := ipnet.IP.Mask(ipnet.Mask).To4()
	if ip == nil {
		return "", fmt.Errorf("bad subnet %q", cidr)
	}
	ip[3] = 1
	return ip.String(), nil
}

func LoadPeerStore(path string) (*PeerStore, error) {
	return LoadPeerStoreWithBase(path, defaultSubnetBase)
}

func LoadPeerStoreWithBase(path string, base int) (*PeerStore, error) {
	subnets, err := subnetsFor(base)
	if err != nil {
		return nil, err
	}
	s := &PeerStore{path: path, Subnets: subnets}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var disk PeerStore
	if err := json.Unmarshal(b, &disk); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	s.Peers = disk.Peers
	if !maps.Equal(disk.Subnets, subnets) && len(s.Peers) > 0 {
		log.Printf("peers: subnet layout changed (%v -> %v), dropping %d stale peers", disk.Subnets, subnets, len(s.Peers))
		s.Peers = nil
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *PeerStore) saveLocked() error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWrite(s.path, b, 0o600)
}

// Allocate assigns a tunnel IP for (pubkey, transport); re-enrolling with an
// existing pubkey+transport returns the same peer (created=false).
func (s *PeerStore) Allocate(pubkey, hostname, transport string) (Peer, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cidr, err := subnetOf(s.Subnets, transport)
	if err != nil {
		return Peer{}, false, err
	}
	for _, p := range s.Peers {
		if p.PubKey == pubkey && p.Transport == transport {
			return p, false, nil
		}
	}
	_, ipnet, _ := net.ParseCIDR(cidr)
	used := map[string]bool{}
	for _, p := range s.Peers {
		used[p.IP] = true
	}
	network := binary.BigEndian.Uint32(ipnet.IP.Mask(ipnet.Mask).To4())
	server := network + 1
	bcast := network | ^binary.BigEndian.Uint32(net.IP(ipnet.Mask).To4())
	for n := network + 1; n < bcast; n++ {
		if n == server {
			continue
		}
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], n)
		candidate := net.IP(b[:]).String()
		if !used[candidate] {
			p := Peer{IP: candidate, PubKey: pubkey, Hostname: hostname, Transport: transport, CreatedAt: time.Now()}
			s.Peers = append(s.Peers, p)
			return p, true, s.saveLocked()
		}
	}
	return Peer{}, false, fmt.Errorf("tunnel IP pool exhausted for %s", transport)
}

func (s *PeerStore) Get(ip string) (Peer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.Peers {
		if p.IP == ip {
			return p, true
		}
	}
	return Peer{}, false
}

func (s *PeerStore) Remove(ip string) (Peer, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, p := range s.Peers {
		if p.IP == ip {
			s.Peers = append(s.Peers[:i], s.Peers[i+1:]...)
			return p, true, s.saveLocked()
		}
	}
	return Peer{}, false, nil
}

func (s *PeerStore) List() []Peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]Peer(nil), s.Peers...)
	sort.Slice(out, func(i, j int) bool { return out[i].IP < out[j].IP })
	return out
}

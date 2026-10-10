package exit

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/huangzheng2016/pvm/internal/fsutil"
)

// Peer is one enrolled gateway/client.
type Peer struct {
	IP        string    `json:"ip"` // tunnel IP, e.g. 10.99.0.2
	PubKey    string    `json:"pubkey"`
	Hostname  string    `json:"hostname"`
	Transport string    `json:"transport"` // udp | tcp
	CreatedAt time.Time `json:"created_at"`
}

// PeerStore allocates tunnel IPs per transport subnet and persists to JSON.
// Subnets derive from a base octet: udp -> 10.<base>.0.0/24,
// tcp -> 10.<base>.1.0/24 (server holds .1). Default base 99.
type PeerStore struct {
	mu      sync.Mutex
	path    string
	subnets map[string]string
	Peers   []Peer `json:"peers"`
}

const defaultSubnetBase = 99

func subnetsFor(base int) map[string]string {
	return map[string]string{
		"udp": fmt.Sprintf("10.%d.0.0/24", base),
		"tcp": fmt.Sprintf("10.%d.1.0/24", base),
	}
}

// SubnetFor returns the CIDR serving a transport (default base, client view).
func SubnetFor(transport string) (string, error) {
	return subnetOf(subnetsFor(defaultSubnetBase), transport)
}

// ServerIP returns the server's address inside a transport subnet (".1"),
// default base (client view).
func ServerIP(transport string) (string, error) {
	return serverIPOf(subnetsFor(defaultSubnetBase), transport)
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
	s := &PeerStore{path: path, subnets: subnetsFor(base)}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
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
	if _, err := subnetOf(s.subnets, transport); err != nil {
		return Peer{}, false, err
	}
	for _, p := range s.Peers {
		if p.PubKey == pubkey && p.Transport == transport {
			return p, false, nil
		}
	}
	cidr, _ := subnetOf(s.subnets, transport)
	_, ipnet, _ := net.ParseCIDR(cidr)
	used := map[string]bool{}
	for _, p := range s.Peers {
		used[p.IP] = true
	}
	base := ipnet.IP.Mask(ipnet.Mask)
	for i := 2; i < 255; i++ {
		candidate := make(net.IP, 4)
		copy(candidate, base)
		candidate[3] = byte(i)
		if !used[candidate.String()] {
			p := Peer{IP: candidate.String(), PubKey: pubkey, Hostname: hostname, Transport: transport, CreatedAt: time.Now()}
			s.Peers = append(s.Peers, p)
			return p, true, s.saveLocked()
		}
	}
	return Peer{}, false, fmt.Errorf("tunnel IP pool exhausted for %s", transport)
}

// Get returns the peer with the given tunnel IP.
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

// Remove deletes the peer with the given tunnel IP. Returns false if absent.
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

// List returns peers sorted by IP.
func (s *PeerStore) List() []Peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]Peer(nil), s.Peers...)
	sort.Slice(out, func(i, j int) bool { return out[i].IP < out[j].IP })
	return out
}

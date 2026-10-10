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
// Subnets: udp -> 10.99.0.0/24, tcp -> 10.99.1.0/24 (server holds .1).
type PeerStore struct {
	mu    sync.Mutex
	path  string
	Peers []Peer `json:"peers"`
}

var transportSubnets = map[string]string{
	"udp": "10.99.0.0/24",
	"tcp": "10.99.1.0/24",
}

// SubnetFor returns the CIDR serving a transport.
func SubnetFor(transport string) (string, error) {
	s, ok := transportSubnets[transport]
	if !ok {
		return "", fmt.Errorf("unknown transport %q", transport)
	}
	return s, nil
}

// ServerIP returns the server's address inside a transport subnet (".1").
func ServerIP(transport string) (string, error) {
	cidr, err := SubnetFor(transport)
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
	s := &PeerStore{path: path}
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
// existing pubkey+transport returns the same peer.
func (s *PeerStore) Allocate(pubkey, hostname, transport string) (Peer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := SubnetFor(transport); err != nil {
		return Peer{}, err
	}
	for _, p := range s.Peers {
		if p.PubKey == pubkey && p.Transport == transport {
			return p, nil
		}
	}
	cidr, _ := SubnetFor(transport)
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
			return p, s.saveLocked()
		}
	}
	return Peer{}, fmt.Errorf("tunnel IP pool exhausted for %s", transport)
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

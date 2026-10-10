package dns

import (
	"container/list"
	"encoding/binary"
	"net"
	"strings"
	"sync"
)

// Pool hands out fake IPs from a CIDR (default 198.18.0.0/16) and keeps
// the two-way domain<->fakeIP mapping. When full, the least recently
// used mapping is evicted and its IP reused.
type Pool struct {
	mu    sync.Mutex
	base  uint32
	count uint32
	max   int
	dToI  map[string]uint32
	iToD  map[uint32]string
	lru   *list.List // of uint32 ip values, front = most recent
	elems map[uint32]*list.Element
	next  uint32 // round-robin cursor for fresh allocations
}

func NewPool(cidr string, maxEntries int) (*Pool, error) {
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, err
	}
	ones, bits := n.Mask.Size()
	count := uint32(1) << (bits - ones)
	if count < 4 {
		return nil, errT("fake-ip pool too small")
	}
	if maxEntries <= 0 {
		maxEntries = int(count) - 2
	}
	return &Pool{
		base:  binary.BigEndian.Uint32(n.IP.To4()),
		count: count,
		max:   maxEntries,
		dToI:  make(map[string]uint32),
		iToD:  make(map[uint32]string),
		lru:   list.New(),
		elems: make(map[uint32]*list.Element),
	}, nil
}

func normDomain(d string) string {
	d = strings.ToLower(strings.TrimSpace(d))
	return strings.TrimSuffix(d, ".")
}

// Allocate returns the fake IP for domain, reusing and refreshing an
// existing mapping when present.
func (p *Pool) Allocate(domain string) net.IP {
	domain = normDomain(domain)
	p.mu.Lock()
	defer p.mu.Unlock()
	if ip, ok := p.dToI[domain]; ok {
		p.lru.MoveToFront(p.elems[ip])
		return uint32ToIP(ip)
	}
	if len(p.dToI) >= p.max {
		p.evictLocked()
	}
	ip := p.freeIPLocked()
	p.dToI[domain] = ip
	p.iToD[ip] = domain
	p.elems[ip] = p.lru.PushFront(ip)
	return uint32ToIP(ip)
}

func (p *Pool) freeIPLocked() uint32 {
	for i := uint32(0); i < p.count-2; i++ {
		// skip network and broadcast offsets
		ip := p.base + 1 + (p.next % (p.count - 2))
		p.next++
		if _, used := p.iToD[ip]; !used {
			return ip
		}
	}
	// every address is mapped; evict the LRU tail and reuse its address
	return p.evictLocked()
}

// evictLocked drops the least recently used mapping and returns its IP.
func (p *Pool) evictLocked() uint32 {
	back := p.lru.Back()
	if back == nil {
		return p.base + 1
	}
	ip := back.Value.(uint32)
	p.lru.Remove(back)
	delete(p.elems, ip)
	delete(p.dToI, p.iToD[ip])
	delete(p.iToD, ip)
	return ip
}

// Lookup resolves a fake IP back to its domain.
func (p *Pool) Lookup(ip net.IP) (string, bool) {
	v := binary.BigEndian.Uint32(ip.To4())
	p.mu.Lock()
	defer p.mu.Unlock()
	d, ok := p.iToD[v]
	return d, ok
}

// Contains reports whether ip belongs to the pool's CIDR.
func (p *Pool) Contains(ip net.IP) bool {
	v := binary.BigEndian.Uint32(ip.To4())
	return v > p.base && v < p.base+p.count-1
}

func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.dToI)
}

// Snapshot returns a copy of all domain -> fake-ip mappings (for status
// dumps like `pvm dns ls`).
func (p *Pool) Snapshot() map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]string, len(p.dToI))
	for d, v := range p.dToI {
		out[d] = uint32ToIP(v).String()
	}
	return out
}

func uint32ToIP(v uint32) net.IP {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return b[:]
}

type errT string

func (e errT) Error() string { return string(e) }

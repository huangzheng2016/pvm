package dns

import (
	"testing"
)

// Pool exhaustion must evict the LRU mapping cleanly: both directions of
// the evicted entry disappear, and the reused IP maps to the new domain.
func TestPoolEvictionKeepsMappingConsistent(t *testing.T) {
	// /30 -> 2 usable addresses; max defaults to count-2.
	p, err := NewPool("198.18.0.0/30", 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.max != 2 {
		t.Fatalf("default max = %d, want 2", p.max)
	}
	ipA := p.Allocate("a.com")
	p.Allocate("b.com")
	ipC := p.Allocate("c.com") // evicts a.com (LRU tail)

	if !ipC.Equal(ipA) {
		t.Fatalf("expected a.com's IP %s to be reused, got %s", ipA, ipC)
	}
	if _, ok := p.Lookup(ipA); !ok {
		t.Fatal("reused IP must map to a domain")
	}
	if d, _ := p.Lookup(ipC); d != "c.com" {
		t.Fatalf("reused IP maps to %q, want c.com", d)
	}
	// the evicted domain must not linger in the forward map: a fresh
	// allocate for it must produce a mapping again, not a ghost.
	if got := p.Allocate("a.com"); got == nil {
		t.Fatal("re-allocate after eviction failed")
	}
	if p.Len() != 2 {
		t.Fatalf("pool len = %d, want 2", p.Len())
	}
}

// When every address is taken, the fallback path evicts the LRU tail and
// reuses its address without leaving stale reverse mappings.
func TestPoolFullFallbackEvictsTail(t *testing.T) {
	p, err := NewPool("198.18.0.0/30", 10) // max larger than the address space
	if err != nil {
		t.Fatal(err)
	}
	ip1 := p.Allocate("one.com")
	p.Allocate("two.com")
	// both addresses used; the next allocate forces the fallback path.
	ip3 := p.Allocate("three.com")
	if !ip3.Equal(ip1) {
		t.Fatalf("fallback should reuse the LRU tail %s, got %s", ip1, ip3)
	}
	if d, ok := p.Lookup(ip3); !ok || d != "three.com" {
		t.Fatalf("Lookup(%s) = %q,%v", ip3, d, ok)
	}
	if p.Len() != 2 {
		t.Fatalf("pool len = %d, want 2", p.Len())
	}
	// and the pool stays consistent under churn
	for i := 0; i < 10; i++ {
		p.Allocate("churn.com")
		ip := p.Allocate("other.com")
		d, ok := p.Lookup(ip)
		if !ok {
			t.Fatal("mapping lost under churn")
		}
		if d != "other.com" && d != "churn.com" {
			t.Fatalf("unexpected domain %q", d)
		}
	}
}

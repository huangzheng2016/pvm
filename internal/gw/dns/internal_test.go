package dns

import (
	"context"
	"sync"
	"testing"

	mdns "github.com/miekg/dns"

	"github.com/huangzheng2016/pvm/internal/gw/rules"
)

// internalStub records which resolver address each query went to.
type internalStub struct {
	mu   sync.Mutex
	seen []string // "addr name"
}

func (s *internalStub) exchange(addr string) Upstream {
	stub := &stubUpstream{}
	return UpstreamFunc(func(ctx context.Context, req *mdns.Msg) (*mdns.Msg, error) {
		s.mu.Lock()
		s.seen = append(s.seen, addr+" "+req.Question[0].Name)
		s.mu.Unlock()
		return stub.Exchange(ctx, req)
	})
}

// UpstreamFunc adapts a function to the Upstream interface (tests).
type UpstreamFunc func(ctx context.Context, req *mdns.Msg) (*mdns.Msg, error)

func (f UpstreamFunc) Exchange(ctx context.Context, req *mdns.Msg) (*mdns.Msg, error) {
	return f(ctx, req)
}

func newInternalRuleEngine(t *testing.T, suffix, dnsAddr string) *rules.Engine {
	t.Helper()
	eng, err := rules.NewEngine("", rules.DefaultInternal())
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Add(rules.Rule{Type: rules.DomainSuffix, Pattern: suffix, Action: rules.Internal, DNS: dnsAddr}); err != nil {
		t.Fatal(err)
	}
	return eng
}

func TestInternalRuleResolvesViaDesignatedServerOnly(t *testing.T) {
	pool, err := NewPool("198.18.0.0/16", 16)
	if err != nil {
		t.Fatal(err)
	}
	def := &stubUpstream{}
	rec := &internalStub{}
	eng := newInternalRuleEngine(t, "corp.test", "10.0.0.53")
	srv := NewServer(def, eng, pool)
	srv.NewInternalUpstream = rec.exchange

	// matched name goes to the designated resolver, answer returned
	resp := query(t, srv, "www.corp.test.", mdns.TypeA)
	if len(resp.Answer) != 1 {
		t.Fatalf("internal name: want 1 answer, got %d", len(resp.Answer))
	}
	if a, ok := resp.Answer[0].(*mdns.A); !ok || a.A.String() != "93.184.216.34" {
		t.Fatalf("internal name: unexpected answer %v", resp.Answer[0])
	}
	rec.mu.Lock()
	seen := append([]string(nil), rec.seen...)
	rec.mu.Unlock()
	if len(seen) != 1 || seen[0] != "10.0.0.53:53 www.corp.test." {
		t.Fatalf("designated resolver saw %v", seen)
	}

	// unmatched name must NOT reach the designated resolver; it gets fake-ip
	resp = query(t, srv, "other.example.", mdns.TypeA)
	if len(resp.Answer) != 1 {
		t.Fatalf("unmatched: want fake-ip answer, got %v", resp.Answer)
	}
	a, ok := resp.Answer[0].(*mdns.A)
	if !ok || !pool.Contains(a.A) {
		t.Fatalf("unmatched: want fake-ip, got %v", resp.Answer[0])
	}
	rec.mu.Lock()
	seen = append([]string(nil), rec.seen...)
	rec.mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("designated resolver must stay untouched for unmatched names: %v", seen)
	}
	// and the default upstream was not consulted either (fake-ip path)
	def.mu.Lock()
	if len(def.queries) != 0 {
		t.Fatalf("default upstream must not see proxied names: %v", def.queries)
	}
	def.mu.Unlock()
}

func TestInternalRuleEmptyDNSYieldsNODATA(t *testing.T) {
	pool, err := NewPool("198.18.0.0/16", 16)
	if err != nil {
		t.Fatal(err)
	}
	def := &stubUpstream{}
	eng := newInternalRuleEngine(t, "corp.test", "")
	srv := NewServer(def, eng, pool)

	resp := query(t, srv, "www.corp.test.", mdns.TypeA)
	if len(resp.Answer) != 0 {
		t.Fatalf("empty dns: want NODATA, got %v", resp.Answer)
	}
	def.mu.Lock()
	defer def.mu.Unlock()
	if len(def.queries) != 0 {
		t.Fatalf("empty dns must never fall back to default upstream: %v", def.queries)
	}
}

func TestInternalRuleHotRemove(t *testing.T) {
	pool, err := NewPool("198.18.0.0/16", 16)
	if err != nil {
		t.Fatal(err)
	}
	rec := &internalStub{}
	eng := newInternalRuleEngine(t, "corp.test", "10.0.0.53")
	srv := NewServer(&stubUpstream{}, eng, pool)
	srv.NewInternalUpstream = rec.exchange

	query(t, srv, "www.corp.test.", mdns.TypeA)
	// remove the rule; the same name must now take the fake-ip path
	list := eng.List()
	if len(list) != 1 {
		t.Fatalf("rules: %v", list)
	}
	if ok, err := eng.Remove(0); !ok || err != nil {
		t.Fatalf("remove: %v %v", ok, err)
	}
	srv.SetRules(eng)
	resp := query(t, srv, "www.corp.test.", mdns.TypeA)
	if len(resp.Answer) != 1 {
		t.Fatalf("after remove: want fake-ip, got %v", resp.Answer)
	}
	if a, ok := resp.Answer[0].(*mdns.A); !ok || !pool.Contains(a.A) {
		t.Fatalf("after remove: want fake-ip, got %v", resp.Answer[0])
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.seen) != 1 {
		t.Fatalf("resolver must not be consulted after rule removal: %v", rec.seen)
	}
}

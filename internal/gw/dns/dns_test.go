package dns

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"

	mdns "github.com/miekg/dns"

	"github.com/huangzheng2016/pvm/internal/gw/rules"
)

type stubUpstream struct {
	mu      sync.Mutex
	queries []string
}

func (s *stubUpstream) Exchange(_ context.Context, req *mdns.Msg) (*mdns.Msg, error) {
	q := req.Question[0]
	s.mu.Lock()
	s.queries = append(s.queries, q.Name)
	s.mu.Unlock()
	resp := mdns.Msg{}
	resp.SetReply(req)
	switch q.Qtype {
	case mdns.TypeA:
		resp.Answer = append(resp.Answer, &mdns.A{
			Hdr: mdns.RR_Header{Name: q.Name, Rrtype: mdns.TypeA, Class: mdns.ClassINET, Ttl: 300},
			A:   net.ParseIP("93.184.216.34"),
		})
	case mdns.TypeAAAA:
		resp.Answer = append(resp.Answer, &mdns.AAAA{
			Hdr:  mdns.RR_Header{Name: q.Name, Rrtype: mdns.TypeAAAA, Class: mdns.ClassINET, Ttl: 300},
			AAAA: net.ParseIP("2606:2800:220:1:248:1893:25c8:1946"),
		})
	}
	return &resp, nil
}

func (s *stubUpstream) queried(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, q := range s.queries {
		if q == name {
			return true
		}
	}
	return false
}

func newTestServer(t *testing.T) (*Server, *stubUpstream, *rules.Engine) {
	t.Helper()
	eng, err := rules.NewEngine("", rules.DefaultInternal())
	if err != nil {
		t.Fatal(err)
	}
	pool, err := NewPool("198.18.0.0/16", 1024)
	if err != nil {
		t.Fatal(err)
	}
	up := &stubUpstream{}
	return NewServer(up, eng, pool), up, eng
}

func query(t *testing.T, srv *Server, name string, qtype uint16) *mdns.Msg {
	t.Helper()
	req := mdns.Msg{}
	req.SetQuestion(mdns.Fqdn(name), qtype)
	rec := &recorder{}
	srv.serve(rec, &req)
	if rec.msg == nil {
		t.Fatal("no response written")
	}
	return rec.msg
}

type recorder struct{ msg *mdns.Msg }

func (r *recorder) LocalAddr() net.Addr        { return nil }
func (r *recorder) RemoteAddr() net.Addr       { return nil }
func (r *recorder) WriteMsg(m *mdns.Msg) error { r.msg = m; return nil }
func (r *recorder) Write(b []byte) (int, error) {
	return 0, errT("unimplemented")
}
func (r *recorder) Close() error        { return nil }
func (r *recorder) TsigStatus() error   { return nil }
func (r *recorder) TsigTimersOnly(bool) {}
func (r *recorder) Hijack()             {}
func (r *recorder) UDP() *net.UDPConn   { return nil }

func TestExternalDomainGetsFakeIP(t *testing.T) {
	srv, up, _ := newTestServer(t)
	resp := query(t, srv, "api.anthropic.com", mdns.TypeA)
	if len(resp.Answer) != 1 {
		t.Fatalf("want 1 answer, got %d", len(resp.Answer))
	}
	a, ok := resp.Answer[0].(*mdns.A)
	if !ok {
		t.Fatal("answer is not A record")
	}
	if !srv.pool.Contains(a.A) {
		t.Fatalf("answer %s not in fake-ip range", a.A)
	}
	if up.queried("api.anthropic.com.") {
		t.Fatal("fake-ip domain must not go upstream")
	}
	domain, ok := srv.pool.Lookup(a.A)
	if !ok || domain != "api.anthropic.com" {
		t.Fatalf("reverse lookup broken: %q ok=%v", domain, ok)
	}
}

func TestInternalDomainAnsweredLocally(t *testing.T) {
	srv, up, _ := newTestServer(t)
	srv.SetStatic("gw.pvm.internal", net.ParseIP("192.168.1.1"))

	resp := query(t, srv, "gw.pvm.internal", mdns.TypeA)
	if len(resp.Answer) != 1 || resp.Answer[0].(*mdns.A).A.String() != "192.168.1.1" {
		t.Fatalf("static internal answer wrong: %+v", resp.Answer)
	}
	if up.queried("gw.pvm.internal.") {
		t.Fatal("internal domain leaked upstream")
	}

	resp = query(t, srv, "nothing.pvm.internal", mdns.TypeA)
	if len(resp.Answer) != 0 || resp.Rcode != mdns.RcodeSuccess {
		t.Fatalf("unknown internal should be NODATA: %+v", resp)
	}
}

func TestDirectRuleGoesUpstream(t *testing.T) {
	srv, up, eng := newTestServer(t)
	if err := eng.Add(rules.Rule{Type: rules.DomainSuffix, Pattern: "corp.example", Action: rules.Direct}); err != nil {
		t.Fatal(err)
	}
	resp := query(t, srv, "git.corp.example", mdns.TypeA)
	if len(resp.Answer) != 1 || resp.Answer[0].(*mdns.A).A.String() != "93.184.216.34" {
		t.Fatalf("direct domain should return real IP: %+v", resp.Answer)
	}
	if !up.queried("git.corp.example.") {
		t.Fatal("direct domain did not reach upstream")
	}
}

func TestFakeIPAfterRuleRemoval(t *testing.T) {
	srv, _, eng := newTestServer(t)
	if err := eng.Add(rules.Rule{Type: rules.DomainSuffix, Pattern: "example.com", Action: rules.Direct}); err != nil {
		t.Fatal(err)
	}
	query(t, srv, "a.example.com", mdns.TypeA)
	if srv.pool.Len() != 0 {
		t.Fatal("direct domain entered fake pool")
	}
	if ok, _ := eng.Remove(0); !ok {
		t.Fatal("remove failed")
	}
	resp := query(t, srv, "a.example.com", mdns.TypeA)
	if len(resp.Answer) != 1 || !srv.pool.Contains(resp.Answer[0].(*mdns.A).A) {
		t.Fatal("domain should become fake-ip after rule removal")
	}
}

func TestAAAAGetsNoDataInFakeMode(t *testing.T) {
	srv, _, _ := newTestServer(t)
	resp := query(t, srv, "api.anthropic.com", mdns.TypeAAAA)
	if len(resp.Answer) != 0 {
		t.Fatalf("AAAA in fake mode should be NODATA: %+v", resp.Answer)
	}
}

func TestPTRReverseLookup(t *testing.T) {
	srv, _, _ := newTestServer(t)
	resp := query(t, srv, "api.anthropic.com", mdns.TypeA)
	fake := resp.Answer[0].(*mdns.A).A

	arpa, err := mdns.ReverseAddr(fake.String())
	if err != nil {
		t.Fatal(err)
	}
	resp = query(t, srv, arpa, mdns.TypePTR)
	if len(resp.Answer) != 1 {
		t.Fatalf("PTR for fake ip should answer: %+v", resp)
	}
	if got := resp.Answer[0].(*mdns.PTR).Ptr; got != "api.anthropic.com." {
		t.Fatalf("PTR wrong: %s", got)
	}
}

func TestOtherTypesForwarded(t *testing.T) {
	srv, up, _ := newTestServer(t)
	query(t, srv, "example.com", mdns.TypeMX)
	if !up.queried("example.com.") {
		t.Fatal("MX should be forwarded upstream")
	}
}

func TestConcurrentQueries(t *testing.T) {
	srv, _, _ := newTestServer(t)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				name := fmt.Sprintf("host-%d-%d.example.com", i, j%5)
				resp := query(t, srv, name, mdns.TypeA)
				if len(resp.Answer) != 1 {
					t.Errorf("no answer for %s", name)
					return
				}
				ip := resp.Answer[0].(*mdns.A).A
				if d, ok := srv.pool.Lookup(ip); !ok || d != name {
					t.Errorf("mapping mismatch for %s -> %s (%q)", name, ip, d)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestPoolLRUEviction(t *testing.T) {
	pool, err := NewPool("198.18.0.0/16", 3)
	if err != nil {
		t.Fatal(err)
	}
	ip1 := pool.Allocate("a.com")
	pool.Allocate("b.com")
	pool.Allocate("c.com")

	// refresh a.com so b.com becomes the oldest
	if got := pool.Allocate("a.com"); !got.Equal(ip1) {
		t.Fatalf("same domain must keep its IP: %s vs %s", got, ip1)
	}
	pool.Allocate("d.com")

	if _, ok := pool.Lookup(ip1); !ok {
		t.Fatal("recently used a.com was evicted")
	}
	found := false
	for _, victim := range []string{"b.com", "c.com", "d.com"} {
		if _, ok := pool.dToI[victim]; !ok {
			found = true
			if victim != "b.com" {
				t.Fatalf("LRU order violated: %s evicted before b.com", victim)
			}
		}
	}
	if !found {
		t.Fatal("no eviction happened at capacity")
	}
	if pool.Len() != 3 {
		t.Fatalf("pool size should stay at max: %d", pool.Len())
	}
}

func TestPoolRejectsBadCIDR(t *testing.T) {
	if _, err := NewPool("198.18.0.0/31", 0); err == nil {
		t.Fatal("tiny pool should be rejected")
	}
	if _, err := NewPool("not-a-cidr", 0); err == nil {
		t.Fatal("bad cidr should be rejected")
	}
}

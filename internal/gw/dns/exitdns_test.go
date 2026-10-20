package dns

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"

	mdns "github.com/miekg/dns"

	"github.com/huangzheng2016/pvm/internal/gw/rules"
)

// srcRecorder tags the query with a source address so the exitFor hook
// can select an exit per VM.
type srcRecorder struct {
	recorder
	addr net.Addr
}

func (r *srcRecorder) RemoteAddr() net.Addr { return r.addr }

func queryFrom(t *testing.T, srv *Server, name string, qtype uint16, src string) *mdns.Msg {
	t.Helper()
	req := mdns.Msg{}
	req.SetQuestion(mdns.Fqdn(name), qtype)
	rec := &srcRecorder{addr: &net.UDPAddr{IP: net.ParseIP(src), Port: 53000}}
	srv.serve(rec, &req)
	if rec.msg == nil {
		t.Fatal("no response written")
	}
	return rec.msg
}

// exitStub records resolver invocations and returns a canned result.
type exitStub struct {
	mu    sync.Mutex
	calls []string // "name@exit"
	ip    net.IP
	mode  ExitResolveMode
	err   error
}

func (s *exitStub) resolve(_ context.Context, name, exit string) (net.IP, ExitResolveMode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, name+"@"+exit)
	return s.ip, s.mode, s.err
}

func (s *exitStub) called() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls) > 0
}

// vmExitMap builds an exitFor hook from a source-IP -> exit table.
func vmExitMap(m map[string]string) func(net.IP) string {
	return func(src net.IP) string {
		if src == nil {
			return ""
		}
		return m[src.String()]
	}
}

func TestExitRoutedNameGetsRealIPFromThatExit(t *testing.T) {
	srv, up, _ := newTestServer(t)
	stub := &exitStub{ip: net.ParseIP("203.0.113.7"), mode: ExitResolveReal}
	srv.SetExitDNS(vmExitMap(map[string]string{"100.99.1.10": "jp"}), stub.resolve)

	resp := queryFrom(t, srv, "api.example.com", mdns.TypeA, "100.99.1.10")
	if len(resp.Answer) != 1 {
		t.Fatalf("want 1 answer, got %d", len(resp.Answer))
	}
	a, ok := resp.Answer[0].(*mdns.A)
	if !ok || a.A.String() != "203.0.113.7" {
		t.Fatalf("want real exit IP 203.0.113.7, got %+v", resp.Answer[0])
	}
	if srv.pool.Contains(a.A) {
		t.Fatal("real answer must not come from the fake pool")
	}
	if a.Hdr.Ttl != srv.fakeTTL {
		t.Fatalf("real answer TTL = %d, want %d", a.Hdr.Ttl, srv.fakeTTL)
	}
	stub.mu.Lock()
	if len(stub.calls) != 1 || stub.calls[0] != "api.example.com@jp" {
		t.Fatalf("resolver calls = %v", stub.calls)
	}
	stub.mu.Unlock()
	if up.queried("api.example.com.") {
		t.Fatal("exit-routed name must not touch the direct upstream")
	}
}

func TestExitSelectionFollowsSourceIP(t *testing.T) {
	srv, _, _ := newTestServer(t)
	resolve := func(_ context.Context, name, exit string) (net.IP, ExitResolveMode, error) {
		switch exit {
		case "jp":
			return net.ParseIP("203.0.113.1"), ExitResolveReal, nil
		case "us":
			return net.ParseIP("203.0.113.2"), ExitResolveReal, nil
		}
		return nil, ExitResolveFake, nil
	}
	srv.SetExitDNS(vmExitMap(map[string]string{
		"100.99.1.10": "jp",
		"100.99.1.11": "us",
	}), resolve)

	resp := queryFrom(t, srv, "a.example.com", mdns.TypeA, "100.99.1.10")
	if got := resp.Answer[0].(*mdns.A).A.String(); got != "203.0.113.1" {
		t.Fatalf("jp vm got %s", got)
	}
	resp = queryFrom(t, srv, "a.example.com", mdns.TypeA, "100.99.1.11")
	if got := resp.Answer[0].(*mdns.A).A.String(); got != "203.0.113.2" {
		t.Fatalf("us vm got %s", got)
	}
	// a source with no exit route gets the direct treatment (fake here)
	resp = queryFrom(t, srv, "a.example.com", mdns.TypeA, "100.99.1.12")
	if got := resp.Answer[0].(*mdns.A).A; !srv.pool.Contains(got) {
		t.Fatalf("direct vm should fall to fake-ip, got %s", got)
	}
}

func TestUnmatchedDirectNameResolvedUpstream(t *testing.T) {
	srv, up, _ := newTestServer(t)
	// production mapping: effective route direct => resolver says Direct
	stub := &exitStub{mode: ExitResolveDirect}
	srv.SetExitDNS(vmExitMap(nil), stub.resolve)

	resp := queryFrom(t, srv, "api.example.com", mdns.TypeA, "100.99.1.10")
	if len(resp.Answer) != 1 || resp.Answer[0].(*mdns.A).A.String() != "93.184.216.34" {
		t.Fatalf("direct default should return the real upstream answer: %+v", resp.Answer)
	}
	if !up.queried("api.example.com.") {
		t.Fatal("direct default must use the existing upstream")
	}
	if srv.pool.Len() != 0 {
		t.Fatal("direct-routed name must not enter the fake pool")
	}
}

func TestExplicitProxyRuleKeepsFakeIP(t *testing.T) {
	srv, up, eng := newTestServer(t)
	if err := eng.Add(rules.Rule{Type: rules.DomainSuffix, Pattern: "example.com", Action: rules.Proxy, Exit: "my-exit"}); err != nil {
		t.Fatal(err)
	}
	stub := &exitStub{ip: net.ParseIP("203.0.113.7"), mode: ExitResolveReal}
	srv.SetExitDNS(vmExitMap(map[string]string{"100.99.1.10": "jp"}), stub.resolve)

	resp := queryFrom(t, srv, "a.example.com", mdns.TypeA, "100.99.1.10")
	if len(resp.Answer) != 1 || !srv.pool.Contains(resp.Answer[0].(*mdns.A).A) {
		t.Fatalf("explicit proxy rule must stay fake-ip: %+v", resp.Answer)
	}
	if stub.called() {
		t.Fatal("explicit proxy rule must not reach the exit resolver")
	}
	if up.queried("a.example.com.") {
		t.Fatal("proxy rule must not leak upstream")
	}
}

func TestDirectAndInternalPrecedenceOverExit(t *testing.T) {
	srv, up, eng := newTestServer(t)
	if err := eng.Add(rules.Rule{Type: rules.DomainSuffix, Pattern: "corp.example", Action: rules.Direct}); err != nil {
		t.Fatal(err)
	}
	if err := eng.Add(rules.Rule{Type: rules.DomainSuffix, Pattern: "corp.test", Action: rules.Internal, DNS: "192.0.2.53:53"}); err != nil {
		t.Fatal(err)
	}
	srv.SetStatic("gw.pvm.internal", net.ParseIP("192.168.1.1"))
	srv.NewInternalUpstream = func(addr string) Upstream { return up }
	stub := &exitStub{ip: net.ParseIP("203.0.113.7"), mode: ExitResolveReal}
	srv.SetExitDNS(vmExitMap(map[string]string{"100.99.1.10": "jp"}), stub.resolve)

	// direct rule: real answer via the direct upstream, resolver untouched
	resp := queryFrom(t, srv, "git.corp.example", mdns.TypeA, "100.99.1.10")
	if len(resp.Answer) != 1 || resp.Answer[0].(*mdns.A).A.String() != "93.184.216.34" {
		t.Fatalf("direct rule must win over the exit route: %+v", resp.Answer)
	}
	// internal suffix: answered locally
	resp = queryFrom(t, srv, "gw.pvm.internal", mdns.TypeA, "100.99.1.10")
	if len(resp.Answer) != 1 || resp.Answer[0].(*mdns.A).A.String() != "192.168.1.1" {
		t.Fatalf("internal name must stay local: %+v", resp.Answer)
	}
	// internal action rule: designated resolver only
	resp = queryFrom(t, srv, "www.corp.test", mdns.TypeA, "100.99.1.10")
	if len(resp.Answer) != 1 || resp.Answer[0].(*mdns.A).A.String() != "93.184.216.34" {
		t.Fatalf("internal rule path broken: %+v", resp.Answer)
	}
	if stub.called() {
		t.Fatal("direct/internal rules must not reach the exit resolver")
	}
	if !up.queried("git.corp.example.") {
		t.Fatal("direct rule did not reach upstream")
	}
}

func TestExitResolverFailureIsServfail(t *testing.T) {
	srv, up, _ := newTestServer(t)
	stub := &exitStub{err: errors.New("tunnel is down")}
	srv.SetExitDNS(vmExitMap(map[string]string{"100.99.1.10": "jp"}), stub.resolve)

	resp := queryFrom(t, srv, "api.example.com", mdns.TypeA, "100.99.1.10")
	if resp.Rcode != mdns.RcodeServerFailure {
		t.Fatalf("resolver failure must be SERVFAIL, got rcode %d", resp.Rcode)
	}
	if len(resp.Answer) != 0 {
		t.Fatalf("SERVFAIL must carry no answers: %+v", resp.Answer)
	}
	if up.queried("api.example.com.") {
		t.Fatal("SERVFAIL must not fall back to the default upstream")
	}
}

func TestProbeFakeModeKeepsReservedAnswer(t *testing.T) {
	srv, _, _ := newTestServer(t)
	stub := &exitStub{mode: ExitResolveFake}
	srv.SetExitDNS(vmExitMap(map[string]string{"100.99.1.10": "my-warp"}), stub.resolve)

	resp := queryFrom(t, srv, "pvm.probe", mdns.TypeA, "100.99.1.10")
	if len(resp.Answer) != 1 || !srv.pool.Contains(resp.Answer[0].(*mdns.A).A) {
		t.Fatalf("reserved probe name must stay fake-ip here: %+v", resp.Answer)
	}
}

func TestProbeRealModeAnswersServerIP(t *testing.T) {
	srv, _, _ := newTestServer(t)
	stub := &exitStub{ip: net.ParseIP("10.7.0.1"), mode: ExitResolveReal}
	srv.SetExitDNS(vmExitMap(map[string]string{"100.99.1.10": "my-exit"}), stub.resolve)

	resp := queryFrom(t, srv, "pvm.probe", mdns.TypeA, "100.99.1.10")
	if len(resp.Answer) != 1 || resp.Answer[0].(*mdns.A).A.String() != "10.7.0.1" {
		t.Fatalf("probe must answer the real in-tunnel server IP: %+v", resp.Answer)
	}
}

func TestAAAAExitRoutedStaysNodata(t *testing.T) {
	srv, up, _ := newTestServer(t)
	stub := &exitStub{ip: net.ParseIP("203.0.113.7"), mode: ExitResolveReal}
	srv.SetExitDNS(vmExitMap(map[string]string{"100.99.1.10": "jp"}), stub.resolve)

	resp := queryFrom(t, srv, "api.example.com", mdns.TypeAAAA, "100.99.1.10")
	if len(resp.Answer) != 0 || resp.Rcode != mdns.RcodeSuccess {
		t.Fatalf("exit-routed AAAA must be NODATA: rcode=%d answers=%+v", resp.Rcode, resp.Answer)
	}
	if stub.called() {
		t.Fatal("AAAA must not reach the exit resolver")
	}
	if up.queried("api.example.com.") {
		t.Fatal("AAAA must not reach the upstream")
	}
}

func TestSetExitDNSNilRestoresLegacyFake(t *testing.T) {
	srv, _, _ := newTestServer(t)
	stub := &exitStub{ip: net.ParseIP("203.0.113.7"), mode: ExitResolveReal}
	srv.SetExitDNS(vmExitMap(map[string]string{"100.99.1.10": "jp"}), stub.resolve)
	srv.SetExitDNS(nil, nil)

	resp := queryFrom(t, srv, "api.example.com", mdns.TypeA, "100.99.1.10")
	if len(resp.Answer) != 1 || !srv.pool.Contains(resp.Answer[0].(*mdns.A).A) {
		t.Fatalf("nil hooks must restore legacy fake-ip: %+v", resp.Answer)
	}
	if stub.called() {
		t.Fatal("nil hooks must disable the resolver")
	}
}

func TestExitPTRDoesNotUseDirectUpstream(t *testing.T) {
	srv, up, _ := newTestServer(t)
	stub := &exitStub{mode: ExitResolveReal}
	srv.SetExitDNS(vmExitMap(map[string]string{"100.99.1.10": "my-exit"}), stub.resolve)
	arpa, err := mdns.ReverseAddr("203.0.113.7")
	if err != nil {
		t.Fatal(err)
	}
	resp := queryFrom(t, srv, arpa, mdns.TypePTR, "100.99.1.10")
	if len(resp.Answer) != 0 {
		t.Fatalf("exit-routed PTR must be NODATA: %+v", resp.Answer)
	}
	if up.queried(mdns.Fqdn(arpa)) {
		t.Fatal("exit-routed PTR must not reach the direct upstream")
	}
}

func TestDirectPTRUsesUpstream(t *testing.T) {
	srv, up, _ := newTestServer(t)
	stub := &exitStub{mode: ExitResolveDirect}
	srv.SetExitDNS(vmExitMap(nil), stub.resolve)
	arpa, err := mdns.ReverseAddr("203.0.113.7")
	if err != nil {
		t.Fatal(err)
	}
	queryFrom(t, srv, arpa, mdns.TypePTR, "100.99.1.10")
	if !up.queried(mdns.Fqdn(arpa)) {
		t.Fatal("direct PTR did not use the direct upstream")
	}
}

func TestDirectCIDRPTRUsesUpstream(t *testing.T) {
	srv, up, eng := newTestServer(t)
	if err := eng.Add(rules.Rule{Type: rules.CIDR, Pattern: "203.0.113.0/24", Action: rules.Direct}); err != nil {
		t.Fatal(err)
	}
	stub := &exitStub{mode: ExitResolveReal}
	srv.SetExitDNS(vmExitMap(map[string]string{"100.99.1.10": "my-exit"}), stub.resolve)
	arpa, err := mdns.ReverseAddr("203.0.113.7")
	if err != nil {
		t.Fatal(err)
	}
	queryFrom(t, srv, arpa, mdns.TypePTR, "100.99.1.10")
	if !up.queried(mdns.Fqdn(arpa)) {
		t.Fatal("direct CIDR PTR did not use the direct upstream")
	}
}

// Package dns implements the gateway DNS relay: internal suffixes are
// answered locally, direct-rule domains are resolved upstream, explicit
// proxy rules get a fake IP whose mapping the gateway uses to route by
// domain at connect time, and unmatched names follow the source VM's
// effective route: a real IPv4 resolved through its exit, or the direct
// upstream when no exit applies.
package dns

import (
	"context"
	"net"
	"strings"
	"sync"
	"time"

	mdns "github.com/miekg/dns"

	"github.com/huangzheng2016/pvm/internal/gw/rules"
)

type Upstream interface {
	Exchange(ctx context.Context, req *mdns.Msg) (*mdns.Msg, error)
}

// ExitResolveMode tells the server how to answer an unmatched name once
// the source VM's effective route is known.
type ExitResolveMode int

const (
	// ExitResolveFake keeps the reserved fake-ip answer: names that must
	// never resolve publicly (pvm.probe) on routes with no in-tunnel
	// target.
	ExitResolveFake ExitResolveMode = iota
	// ExitResolveDirect answers via the server's normal direct upstream.
	ExitResolveDirect
	// ExitResolveReal answers with the returned real IPv4, which was
	// resolved through the source VM's exit.
	ExitResolveReal
)

// ExitResolver resolves an unmatched name for the given exit ("" = the
// source VM's effective route is direct). The IP is only meaningful with
// ExitResolveReal. An error means SERVFAIL: exit-side resolution must
// never fall back to the host's resolver.
type ExitResolver func(ctx context.Context, name, exit string) (net.IP, ExitResolveMode, error)

type UDPUpstream struct{ Addr string }

func (u UDPUpstream) Exchange(ctx context.Context, req *mdns.Msg) (*mdns.Msg, error) {
	c := &mdns.Client{Net: "udp", Timeout: 5 * time.Second}
	resp, _, err := c.ExchangeContext(ctx, req, u.Addr)
	if err != nil || resp == nil || !resp.Truncated {
		return resp, err
	}
	// Large answers (DNSSEC, many records) must not be forwarded forever
	// in their truncated UDP form: retry the same query over TCP.
	c.Net = "tcp"
	resp, _, err = c.ExchangeContext(ctx, req, u.Addr)
	return resp, err
}

type Server struct {
	upstream Upstream
	pool     *Pool
	fakeTTL  uint32

	// NewInternalUpstream builds the upstream for an internal-rule
	// resolver address; replaceable in tests to observe isolation.
	NewInternalUpstream func(addr string) Upstream

	// OnInternalAnswer, when set, is invoked with the question name and
	// the upstream answer for internal-rule forwards (both the default
	// uplink and designated-resolver paths), so the routing layer can
	// learn bypass IPs for internal domains.
	OnInternalAnswer func(name string, resp *mdns.Msg)

	// exitFor maps a query's source IP to the effective exit name of the
	// VM ("" = direct); exitResolver then decides how an unmatched name
	// is answered for that route. Both nil keeps the legacy mode where
	// every unmatched name gets a fake IP.
	exitFor     func(sourceIP net.IP) string
	exitResolve ExitResolver

	mu     sync.RWMutex
	rules  *rules.Engine
	static map[string]net.IP
	svrs   []*mdns.Server
}

func NewServer(upstream Upstream, eng *rules.Engine, pool *Pool) *Server {
	return &Server{
		upstream: upstream,
		rules:    eng,
		pool:     pool,
		static:   make(map[string]net.IP),
		fakeTTL:  5,
	}
}

// SetRules hot-swaps the routing rules engine; in-flight queries finish
// with the old engine.
func (s *Server) SetRules(eng *rules.Engine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = eng
}

func (s *Server) Rules() *rules.Engine {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rules
}

func (s *Server) SetStatic(name string, ip net.IP) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.static[strings.ToLower(strings.TrimSuffix(name, "."))] = ip
}

// SetExitDNS installs the exit-routed real-IP path: exitFor maps the
// query's source IP to the effective exit name ("" = direct) and
// resolver answers unmatched names for that route. Passing nil, nil
// restores the legacy all-fake behavior.
func (s *Server) SetExitDNS(exitFor func(sourceIP net.IP) string, resolver ExitResolver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exitFor, s.exitResolve = exitFor, resolver
}

func (s *Server) exitDNS() (func(net.IP) string, ExitResolver) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.exitFor, s.exitResolve
}

func (s *Server) Pool() *Pool { return s.pool }

func (s *Server) ListenAndServe(addr string) error {
	handler := mdns.HandlerFunc(s.serve)
	errc := make(chan error, 2)
	for _, network := range []string{"udp", "tcp"} {
		srv := &mdns.Server{Addr: addr, Net: network, Handler: handler}
		s.mu.Lock()
		s.svrs = append(s.svrs, srv)
		s.mu.Unlock()
		go func() {
			if err := srv.ListenAndServe(); err != nil {
				errc <- err
			}
		}()
	}
	return <-errc
}

func (s *Server) Shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, srv := range s.svrs {
		srv.Shutdown()
	}
}

func (s *Server) serve(w mdns.ResponseWriter, req *mdns.Msg) {
	resp := mdns.Msg{}
	resp.SetReply(req)
	resp.Authoritative = false
	resp.RecursionAvailable = true

	if len(req.Question) == 0 {
		w.WriteMsg(&resp)
		return
	}
	q := req.Question[0]
	name := strings.ToLower(strings.TrimSuffix(q.Name, "."))

	switch q.Qtype {
	case mdns.TypeA, mdns.TypeAAAA:
		s.answerHost(req, &resp, name, q.Qtype, sourceIP(w))
	case mdns.TypePTR:
		s.answerPTR(req, &resp, name, sourceIP(w))
	default:
		s.answerOther(req, &resp, name)
	}
	w.WriteMsg(&resp)
}

// sourceIP extracts the query's source IP from the response writer;
// nil when the transport does not expose one.
func sourceIP(w mdns.ResponseWriter) net.IP {
	switch a := w.RemoteAddr().(type) {
	case *net.UDPAddr:
		return a.IP
	case *net.TCPAddr:
		return a.IP
	}
	return nil
}

func (s *Server) answerOther(req, resp *mdns.Msg, name string) {
	if s.Rules().IsInternal(name) {
		return
	}
	if d, ok := s.Rules().Match(name, nil); ok && !d.Internal {
		switch d.Action {
		case rules.Internal:
			if d.DNS == "" {
				s.forward(req, resp)
			} else {
				s.forwardInternal(req, resp, d.DNS)
			}
		case rules.Direct:
			s.forward(req, resp)
		}
	}
}

func (s *Server) answerHost(req, resp *mdns.Msg, name string, qtype uint16, srcIP net.IP) {
	if s.Rules().IsInternal(name) {
		// internal names never go upstream: static record or NODATA
		if ip, ok := s.static[name]; ok && qtype == mdns.TypeA {
			resp.Answer = append(resp.Answer, &mdns.A{
				Hdr: mdns.RR_Header{Name: req.Question[0].Name, Rrtype: mdns.TypeA, Class: mdns.ClassINET, Ttl: 60},
				A:   ip,
			})
		}
		return
	}
	if d, ok := s.Rules().Match(name, nil); ok && !d.Internal {
		switch d.Action {
		case rules.Internal:
			if d.DNS == "" {
				// no designated resolver: internal names take the
				// default uplink (host DNS resolves corp/VPN names)
				s.forward(req, resp)
			} else {
				// designated resolver: ONLY that server ever sees the name
				s.forwardInternal(req, resp, d.DNS)
			}
			if s.OnInternalAnswer != nil {
				s.OnInternalAnswer(name, resp)
			}
			return
		case rules.Direct:
			s.forward(req, resp)
			return
		case rules.Proxy:
			// explicit split rules keep fake-ip; the exit resolver below
			// only ever sees unmatched names.
			s.answerFake(req, resp, name, qtype)
			return
		}
	}
	exitFor, resolve := s.exitDNS()
	if resolve == nil {
		s.answerFake(req, resp, name, qtype)
		return
	}
	if qtype == mdns.TypeAAAA {
		// v4-only model: exit-routed traffic never gets AAAA, and the
		// direct path keeps its historical NODATA fallback to A.
		return
	}
	exit := ""
	if exitFor != nil {
		exit = exitFor(srcIP)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ip, mode, err := resolve(ctx, name, exit)
	if err != nil {
		// tunnel down or in-tunnel resolution failure: fail closed,
		// never fall back to the host's resolver.
		resp.Rcode = mdns.RcodeServerFailure
		return
	}
	switch mode {
	case ExitResolveDirect:
		s.forward(req, resp)
	case ExitResolveReal:
		if ip == nil {
			resp.Rcode = mdns.RcodeServerFailure
			return
		}
		resp.Answer = append(resp.Answer, &mdns.A{
			Hdr: mdns.RR_Header{Name: req.Question[0].Name, Rrtype: mdns.TypeA, Class: mdns.ClassINET, Ttl: s.fakeTTL},
			A:   ip,
		})
	default: // ExitResolveFake
		s.answerFake(req, resp, name, qtype)
	}
}

// answerFake hands out a fake IP for A queries; AAAA stays NODATA so the
// client falls back to A.
func (s *Server) answerFake(req, resp *mdns.Msg, name string, qtype uint16) {
	if qtype == mdns.TypeAAAA {
		return
	}
	ip := s.pool.Allocate(name)
	resp.Answer = append(resp.Answer, &mdns.A{
		Hdr: mdns.RR_Header{Name: req.Question[0].Name, Rrtype: mdns.TypeA, Class: mdns.ClassINET, Ttl: s.fakeTTL},
		A:   ip,
	})
}

func (s *Server) answerPTR(req, resp *mdns.Msg, arpa string, srcIP net.IP) {
	ip := arpaToIP(arpa)
	if ip != nil && s.pool.Contains(ip) {
		if domain, ok := s.pool.Lookup(ip); ok {
			resp.Answer = append(resp.Answer, &mdns.PTR{
				Hdr: mdns.RR_Header{Name: req.Question[0].Name, Rrtype: mdns.TypePTR, Class: mdns.ClassINET, Ttl: s.fakeTTL},
				Ptr: mdns.Fqdn(domain),
			})
		} else {
			resp.Rcode = mdns.RcodeNameError
		}
		return
	}
	if d, ok := s.Rules().Match("", ip); ok && !d.Internal {
		switch d.Action {
		case rules.Internal:
			if d.DNS == "" {
				s.forward(req, resp)
			} else {
				s.forwardInternal(req, resp, d.DNS)
			}
		case rules.Direct:
			s.forward(req, resp)
		}
		return
	}
	// There is no in-tunnel PTR resolver. Return NODATA for exit-routed
	// real IPs instead of leaking the reverse lookup to the host resolver.
	if exitFor, _ := s.exitDNS(); exitFor != nil && exitFor(srcIP) != "" {
		return
	}
	s.forward(req, resp)
}

func arpaToIP(arpa string) net.IP {
	const suffix = ".in-addr.arpa"
	if !strings.HasSuffix(arpa, suffix) {
		return nil
	}
	parts := strings.Split(strings.TrimSuffix(arpa, suffix), ".")
	if len(parts) != 4 {
		return nil
	}
	ip := make(net.IP, 4)
	for i, p := range parts {
		n, err := atoi(p)
		if err != nil || n > 255 {
			return nil
		}
		ip[3-i] = byte(n)
	}
	return ip
}

func atoi(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, errT("empty octet")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errT("bad octet")
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

func (s *Server) forward(req, resp *mdns.Msg) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	up, err := s.upstream.Exchange(ctx, req)
	if err != nil {
		resp.Rcode = mdns.RcodeServerFailure
		return
	}
	*resp = *up
	resp.Id = req.Id
}

// forwardInternal resolves via the internal rule's designated resolver;
// only names matching the rule may ever reach that server.
func (s *Server) forwardInternal(req, resp *mdns.Msg, addr string) {
	if addr == "" {
		resp.Rcode = mdns.RcodeServerFailure
		return
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, "53")
	}
	mk := s.NewInternalUpstream
	if mk == nil {
		mk = func(a string) Upstream { return UDPUpstream{Addr: a} }
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	up, err := mk(addr).Exchange(ctx, req)
	if err != nil {
		resp.Rcode = mdns.RcodeServerFailure
		return
	}
	*resp = *up
	resp.Id = req.Id
}

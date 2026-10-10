// Package dns implements the gateway DNS relay: internal suffixes are
// answered locally, direct-rule domains are resolved upstream, and all
// other names get a fake IP whose mapping the gateway uses to route by
// domain at connect time.
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

// Upstream resolves queries the relay does not answer itself.
type Upstream interface {
	Exchange(ctx context.Context, req *mdns.Msg) (*mdns.Msg, error)
}

// UDPUpstream forwards to a plain DNS server (e.g. slirp's 10.0.2.3).
type UDPUpstream struct{ Addr string }

func (u UDPUpstream) Exchange(ctx context.Context, req *mdns.Msg) (*mdns.Msg, error) {
	c := &mdns.Client{Net: "udp", Timeout: 5 * time.Second}
	resp, _, err := c.ExchangeContext(ctx, req, u.Addr)
	return resp, err
}

type Server struct {
	upstream Upstream
	pool     *Pool
	fakeTTL  uint32

	// NewInternalUpstream builds the upstream for an internal-rule
	// resolver address; replaceable in tests to observe isolation.
	NewInternalUpstream func(addr string) Upstream

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

// Rules returns the current rules engine.
func (s *Server) Rules() *rules.Engine {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rules
}

// SetStatic pins an internal name (e.g. gw.pvm.internal) to a fixed IP.
func (s *Server) SetStatic(name string, ip net.IP) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.static[strings.ToLower(strings.TrimSuffix(name, "."))] = ip
}

func (s *Server) Pool() *Pool { return s.pool }

// ListenAndServe starts UDP and TCP listeners on addr and blocks until
// Shutdown or a listener error.
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
		s.answerHost(req, &resp, name, q.Qtype)
	case mdns.TypePTR:
		s.answerPTR(req, &resp, name)
	default:
		s.forward(req, &resp)
	}
	w.WriteMsg(&resp)
}

func (s *Server) answerHost(req, resp *mdns.Msg, name string, qtype uint16) {
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
				return
			}
			// designated resolver: ONLY that server ever sees the name
			s.forwardInternal(req, resp, d.DNS)
			return
		case rules.Direct:
			s.forward(req, resp)
			return
		}
	}
	if qtype == mdns.TypeAAAA {
		// fake-IP is v4-only; NODATA makes the client fall back to A
		return
	}
	ip := s.pool.Allocate(name)
	resp.Answer = append(resp.Answer, &mdns.A{
		Hdr: mdns.RR_Header{Name: req.Question[0].Name, Rrtype: mdns.TypeA, Class: mdns.ClassINET, Ttl: s.fakeTTL},
		A:   ip,
	})
}

func (s *Server) answerPTR(req, resp *mdns.Msg, arpa string) {
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

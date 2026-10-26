package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	mdns "github.com/miekg/dns"

	"github.com/huangzheng2016/pvm/internal/exit"
	gwdns "github.com/huangzheng2016/pvm/internal/gw/dns"
	gwpolicy "github.com/huangzheng2016/pvm/internal/gw/policy"
	"github.com/huangzheng2016/pvm/internal/gw/rules"
)

// fakeDNS owns the cluster DNS service: internal suffixes and direct
// rules resolve upstream, explicit proxy rules get a fake IP from the
// pool (198.18.0.0/16), and unmatched names follow the source VM's
// effective route once exit hooks are installed: a real IPv4 resolved
// through its exit, or the direct upstream when no exit applies. The
// pool outlives config reloads so existing connections keep their
// mappings; only the rules engine is swapped.
type fakeDNS struct {
	pool *gwdns.Pool
	srv  *gwdns.Server

	onRulesSwap func(*rules.Engine)

	// exit-DNS hooks, installed by SetExitHooks on every config
	// generation; unset they keep the legacy all-fake behavior.
	markFor    func(string) int
	dcAddrFor  func(string) (string, bool)
	dnsAddrFor func(string) (string, bool)
	// resolveExit is the injectable in-tunnel lookup; nil uses the
	// production SO_MARK + domain-connect / in-tunnel DNS path.
	resolveExit func(ctx context.Context, domain, exitName string, mark int, dcAddr, dnsAddr string) (net.IP, error)

	mu     sync.Mutex
	done   chan struct{}
	closed bool
}

func newFakeDNS(upstream string, onDirectIP func(domain string, ips []string)) (*fakeDNS, error) {
	pool, err := gwdns.NewPool("198.18.0.0/16", 0)
	if err != nil {
		return nil, err
	}
	eng, err := rules.NewEngine("", rules.DefaultInternal())
	if err != nil {
		return nil, err
	}
	up := gwdns.Upstream(gwdns.UDPUpstream{Addr: upstream + ":53"})
	var engPtr *atomic.Pointer[rules.Engine]
	if onDirectIP != nil {
		engPtr = &atomic.Pointer[rules.Engine]{}
		engPtr.Store(eng)
		up = &bypassRecorder{inner: up, engine: engPtr, cb: onDirectIP}
	}
	srv := gwdns.NewServer(up, eng, pool)
	if onDirectIP != nil {
		// internal-rule answers (either uplink path) are learned too:
		// their traffic must bypass the tunnel like direct domains.
		srv.OnInternalAnswer = func(name string, resp *mdns.Msg) {
			var ips []string
			for _, rr := range resp.Answer {
				if a, ok := rr.(*mdns.A); ok {
					ips = append(ips, a.A.String())
				}
			}
			if len(ips) > 0 {
				onDirectIP(name, ips)
			}
		}
	}
	f := &fakeDNS{pool: pool, srv: srv, done: make(chan struct{})}
	// the recorder must follow rules hot-swaps
	f.onRulesSwap = func(newEng *rules.Engine) {
		if engPtr != nil {
			engPtr.Store(newEng)
		}
	}
	return f, nil
}

func (f *fakeDNS) Start(bindIP string) error {
	errc := make(chan error, 1)
	go func() {
		if err := f.srv.ListenAndServe(net.JoinHostPort(bindIP, "53")); err != nil {
			errc <- err
		}
	}()
	select {
	case err := <-errc:
		return err
	case <-time.After(300 * time.Millisecond):
		return nil
	}
}

func (f *fakeDNS) SetRules(rs []rules.Rule) error {
	eng, err := rules.NewEngine("", rules.DefaultInternal())
	if err != nil {
		return err
	}
	for _, r := range rs {
		if err := eng.Add(r); err != nil {
			return fmt.Errorf("rule %+v: %w", r, err)
		}
	}
	f.srv.SetRules(eng)
	if f.onRulesSwap != nil {
		f.onRulesSwap(eng)
	}
	return nil
}

func (f *fakeDNS) Rules() *rules.Engine { return f.srv.Rules() }

func (f *fakeDNS) Pool() *gwdns.Pool { return f.pool }

// SetExitHooks installs the exit-DNS path with the current policy and
// tunnel lookups; the agent calls it on every config generation so rule
// and route changes apply to DNS immediately.
func (f *fakeDNS) SetExitHooks(pol *gwpolicy.Engine, vmByIP func(net.IP) string, markFor func(string) int, dcAddrFor, dnsAddrFor func(string) (string, bool)) {
	if markFor == nil {
		markFor = func(string) int { return -1 }
	}
	if dcAddrFor == nil {
		dcAddrFor = func(string) (string, bool) { return "", false }
	}
	if dnsAddrFor == nil {
		dnsAddrFor = func(string) (string, bool) { return "", false }
	}
	f.mu.Lock()
	f.markFor, f.dcAddrFor, f.dnsAddrFor = markFor, dcAddrFor, dnsAddrFor
	f.mu.Unlock()
	f.srv.SetExitDNS(exitForSource(pol, vmByIP), f.exitResolver())
}

// exitForSource builds the source-IP -> effective-exit mapper handed to
// the DNS server ("" = direct route).
func exitForSource(pol *gwpolicy.Engine, vmByIP func(net.IP) string) func(net.IP) string {
	return func(srcIP net.IP) string {
		if pol == nil || vmByIP == nil {
			return ""
		}
		return pol.ExitForVM(vmByIP(srcIP))
	}
}

// exitHooks snapshots the tunnel lookups under the mutex; DNS queries
// run on server goroutines and race with config-generation swaps.
func (f *fakeDNS) exitHooks() (func(string) int, func(string) (string, bool), func(string) (string, bool)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.markFor, f.dcAddrFor, f.dnsAddrFor
}

// exitResolver builds the resolver handed to the DNS server; tests
// invoke it directly.
func (f *fakeDNS) exitResolver() gwdns.ExitResolver {
	return func(ctx context.Context, name, exit string) (net.IP, gwdns.ExitResolveMode, error) {
		return f.resolveThroughExit(ctx, name, exit, f.resolveExit)
	}
}

// resolveThroughExit answers an unmatched name for a source VM whose
// effective route is exit ("" = direct): a real IPv4 resolved through
// the exit's tunnel, the reserved fake-ip for probe names without an
// agent, the direct upstream for direct routes, and an error (SERVFAIL)
// on tunnel failure — never a host-resolver fallback.
func (f *fakeDNS) resolveThroughExit(ctx context.Context, name, exit string, lookup func(context.Context, string, string, int, string, string) (net.IP, error)) (net.IP, gwdns.ExitResolveMode, error) {
	markFor, dcAddrFor, dnsAddrFor := f.exitHooks()
	if name == probeDomain {
		if exit != "" && markFor(exit) < 0 {
			return nil, 0, fmt.Errorf("exit %s: tunnel is down", exit)
		}
		// pvm.probe never resolves publicly: agent exits answer with the
		// server's real in-tunnel address so the UDP/ICMP probes exercise
		// L3 forwarding; direct/WARP keep the reserved fake answer.
		if ip, err := probeAddress(name, exit, dcAddrFor); err == nil && ip != nil {
			return ip, gwdns.ExitResolveReal, nil
		}
		return nil, gwdns.ExitResolveFake, nil
	}
	if exit == "" {
		return nil, gwdns.ExitResolveDirect, nil
	}
	mark := markFor(exit)
	if mark < 0 {
		return nil, 0, fmt.Errorf("exit %s: tunnel is down", exit)
	}
	dcAddr, _ := dcAddrFor(exit)
	dnsAddr, _ := dnsAddrFor(exit)
	if lookup == nil {
		lookup = resolveViaExitTunnel
	}
	ip, err := lookup(ctx, name, exit, mark, dcAddr, dnsAddr)
	if err != nil {
		return nil, 0, err
	}
	return ip, gwdns.ExitResolveReal, nil
}

// resolveViaExitTunnel resolves a name through one exit's tunnel: the
// exit-agent's domain-connect resolve service when available, else the
// exit's in-tunnel DNS (WARP). Both dial with the exit's SO_MARK so the
// query packets enter the right WireGuard table.
func resolveViaExitTunnel(ctx context.Context, domain, exitName string, mark int, dcAddr, dnsAddr string) (net.IP, error) {
	if dcAddr != "" {
		d := net.Dialer{Timeout: 10 * time.Second, Control: soMark(mark)}
		conn, err := d.DialContext(ctx, "tcp4", dcAddr)
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		return exit.WriteDomainResolve(conn, domain)
	}
	if dnsAddr != "" {
		resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second, Control: soMark(mark)}
			return d.DialContext(ctx, "udp", dnsAddr)
		}}
		ips, err := resolver.LookupIP(ctx, "ip4", domain)
		return firstIPv4(ips, err)
	}
	return nil, fmt.Errorf("no in-tunnel resolver for %s", exitName)
}

func (f *fakeDNS) DumpMappings(path string) {
	m := f.pool.Snapshot()
	out := map[string]string{}
	for dom, ip := range m {
		out[dom] = ip
	}
	data, _ := json.MarshalIndent(out, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err == nil {
		os.Rename(tmp, path)
	}
}

// RunDumpLoop refreshes the mapping dump every few seconds until Close.
func (f *fakeDNS) RunDumpLoop(path string, interval time.Duration) {
	go func() {
		for {
			f.DumpMappings(path)
			select {
			case <-f.done:
				return
			case <-time.After(interval):
			}
		}
	}()
}

func (f *fakeDNS) Close() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	close(f.done)
	f.mu.Unlock()
	f.srv.Shutdown()
}

var _ = log.Printf // keep log import if unused in future edits

// bypassRecorder wraps the DNS upstream: A records of direct-rule
// answers are reported so the routing layer can pin those IPs to the
// main table (direct domains bypass the tunnel even for proxied VMs).
// Internal-rule answers are learned separately via Server.OnInternalAnswer.
type bypassRecorder struct {
	inner  gwdns.Upstream
	engine *atomic.Pointer[rules.Engine]
	cb     func(domain string, ips []string)
}

func (b *bypassRecorder) Exchange(ctx context.Context, req *mdns.Msg) (*mdns.Msg, error) {
	resp, err := b.inner.Exchange(ctx, req)
	if err != nil || resp == nil || len(req.Question) == 0 {
		return resp, err
	}
	domain := strings.ToLower(strings.TrimSuffix(req.Question[0].Name, "."))
	if d, ok := b.engine.Load().Match(domain, nil); !ok || d.Action != rules.Direct || d.Internal {
		return resp, err
	}
	var ips []string
	for _, rr := range resp.Answer {
		if a, ok := rr.(*mdns.A); ok {
			ips = append(ips, a.A.String())
		}
	}
	if len(ips) > 0 {
		b.cb(domain, ips)
	}
	return resp, err
}

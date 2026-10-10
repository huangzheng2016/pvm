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

	gwdns "github.com/huangzheng2016/pvm/internal/gw/dns"
	"github.com/huangzheng2016/pvm/internal/gw/rules"
)

// fakeDNS owns the cluster DNS service: internal suffixes and direct
// rules resolve upstream, everything else gets a fake IP from the pool
// (198.18.0.0/16). The pool outlives config reloads so existing
// connections keep their mappings; only the rules engine is swapped.
type fakeDNS struct {
	pool *gwdns.Pool
	srv  *gwdns.Server

	onRulesSwap func(*rules.Engine)

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

// SetRules swaps in a freshly compiled engine built from the config.
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

// Rules returns the current engine (shared with the flow policy).
func (f *fakeDNS) Rules() *rules.Engine { return f.srv.Rules() }

// Pool exposes the fake-ip pool for the transparent proxy.
func (f *fakeDNS) Pool() *gwdns.Pool { return f.pool }

// DumpMappings writes the domain<->fake-ip view for `pvm dns ls`.
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
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.closed = true
	close(f.done)
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

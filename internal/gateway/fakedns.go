package gateway

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

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

	mu     sync.Mutex
	done   chan struct{}
	closed bool
}

func newFakeDNS(upstream string) (*fakeDNS, error) {
	pool, err := gwdns.NewPool("198.18.0.0/16", 1<<16)
	if err != nil {
		return nil, err
	}
	eng, err := rules.NewEngine("", rules.DefaultInternal())
	if err != nil {
		return nil, err
	}
	srv := gwdns.NewServer(gwdns.UDPUpstream{Addr: upstream + ":53"}, eng, pool)
	return &fakeDNS{pool: pool, srv: srv, done: make(chan struct{})}, nil
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

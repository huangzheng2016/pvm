// Package policy turns DNS results and routing rules into a per-flow
// decision for the gateway's data plane.
package policy

import (
	"net"
	"sync"

	"github.com/huangzheng2016/pvm/internal/gw/dns"
	"github.com/huangzheng2016/pvm/internal/gw/rules"
)

type Route int

const (
	Direct Route = iota
	Proxy
)

func (r Route) String() string {
	if r == Proxy {
		return "proxy"
	}
	return "direct"
}

type Decision struct {
	Route Route
	Exit  string // exit-node name when Route == Proxy
}

type Engine struct {
	rules *rules.Engine
	pool  *dns.Pool

	mu      sync.RWMutex
	vmExit  map[string]string // vmID -> default exit name, "" = direct
	defExit string            // cluster-wide default exit, "" = direct
}

func New(eng *rules.Engine, pool *dns.Pool) *Engine {
	return &Engine{rules: eng, pool: pool, vmExit: make(map[string]string)}
}

// SetVMExit pins a VM's default route; exit "" restores direct egress.
func (e *Engine) SetVMExit(vmID, exit string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if exit == "" {
		delete(e.vmExit, vmID)
	} else {
		e.vmExit[vmID] = exit
	}
}

// SetDefaultExit sets the cluster-wide fallback used when neither a
// rule nor a per-VM override matches.
func (e *Engine) SetDefaultExit(exit string) { e.defExit = exit }

// LookupByFakeIP resolves a fake IP back to the domain that produced
// it, so proxied connections can be routed by name (e.g. SNI-less or
// hard-coded DNS clients stay correct).
func (e *Engine) LookupByFakeIP(ip net.IP) (string, bool) {
	return e.pool.Lookup(ip)
}

// IsFakeIP reports whether ip came from the fake-ip pool.
func (e *Engine) IsFakeIP(ip net.IP) bool { return e.pool.Contains(ip) }

// DecideFlow decides where a flow from vmID goes. domain may be ""
// (then dstIP is reverse-resolved through the fake-ip pool); dstIP may
// be nil when the caller only has a name.
func (e *Engine) DecideFlow(domain string, dstIP net.IP, vmID string) Decision {
	if domain == "" && dstIP != nil && e.pool.Contains(dstIP) {
		domain, _ = e.pool.Lookup(dstIP)
	}
	if d, ok := e.rules.Match(domain, dstIP); ok {
		if d.Internal || d.Action == rules.Direct {
			return Decision{Route: Direct}
		}
		return Decision{Route: Proxy, Exit: d.Exit}
	}
	e.mu.RLock()
	exit, ok := e.vmExit[vmID]
	def := e.defExit
	e.mu.RUnlock()
	if !ok {
		exit = def
	}
	if exit == "" {
		return Decision{Route: Direct}
	}
	return Decision{Route: Proxy, Exit: exit}
}

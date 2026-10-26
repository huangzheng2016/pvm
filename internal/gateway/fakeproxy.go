package gateway

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/huangzheng2016/pvm/internal/exit"
	gwpolicy "github.com/huangzheng2016/pvm/internal/gw/policy"
)

// fakeProxy terminates TCP connections the nft rules REDIRECT from the
// fake-ip range: the original destination (a fake IP) is recovered with
// SO_ORIGINAL_DST, mapped back to its domain, routed by policy and
// dialed out. Direct flows use the uplink resolver. Proxied flows use
// either the exit-agent's domain-connect service or a resolver reached
// through the tunnel; the host resolver is never used for them.
type fakeProxy struct {
	policy     *gwpolicy.Engine
	resolver   *net.Resolver
	vmByIP     func(ip net.IP) string               // config lookup, nil-safe
	markFor    func(exitName string) int            // SO_MARK selecting the exit's table; -1 = no tunnel
	dcAddrFor  func(exitName string) (string, bool) // in-tunnel domain-connect addr
	dnsAddrFor func(exitName string) (string, bool) // in-tunnel resolver for exits without domain-connect

	mu     sync.Mutex
	ln     *net.TCPListener
	closed bool
	wg     sync.WaitGroup
	conns  map[*trackedConn]struct{} // live relay connections, for route-flip resets
}

// trackedConn is one live relayed flow. On a policy hot-swap, flows
// whose routing decision flipped (direct<->proxy, or proxy exit gone)
// are reset so they reconnect under the new route instead of leaking
// on the old path indefinitely.
type trackedConn struct {
	vm, domain string
	route      gwpolicy.Route
	exit       string
	src, up    net.Conn
}

func newFakeProxy(policy *gwpolicy.Engine, upstream string, vmByIP func(net.IP) string) *fakeProxy {
	return &fakeProxy{
		policy: policy,
		resolver: &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				d := net.Dialer{Timeout: 5 * time.Second}
				return d.DialContext(ctx, "udp", upstream+":53")
			},
		},
		vmByIP:     vmByIP,
		markFor:    func(string) int { return -1 },
		dcAddrFor:  func(string) (string, bool) { return "", false },
		dnsAddrFor: func(string) (string, bool) { return "", false },
		conns:      map[*trackedConn]struct{}{},
	}
}

// SetPolicy swaps the flow policy on config reload, then resets every
// live flow whose decision flipped under the new policy: they must
// re-dial via the new route, not keep leaking on the old one.
func (p *fakeProxy) SetPolicy(eng *gwpolicy.Engine) {
	p.mu.Lock()
	p.policy = eng
	var kill []*trackedConn
	for tc := range p.conns {
		dec := eng.DecideFlow(tc.domain, nil, tc.vm)
		if dec.Route != tc.route || dec.Exit != tc.exit {
			kill = append(kill, tc)
		}
	}
	p.mu.Unlock()
	for _, tc := range kill {
		tc.src.Close()
		tc.up.Close()
	}
}

func (p *fakeProxy) curPolicy() *gwpolicy.Engine {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.policy
}

// SetExitHooks installs the tunnel lookup hooks on config reload, then
// resets proxied flows whose exit tunnel is now down: the flow is dead
// anyway, so the VM gets an immediate RST instead of a hanging relay.
func (p *fakeProxy) SetExitHooks(markFor func(string) int, dcAddrFor, dnsAddrFor func(string) (string, bool)) {
	p.mu.Lock()
	p.markFor, p.dcAddrFor, p.dnsAddrFor = markFor, dcAddrFor, dnsAddrFor
	var kill []*trackedConn
	for tc := range p.conns {
		if tc.route == gwpolicy.Proxy && markFor(tc.exit) < 0 {
			kill = append(kill, tc)
		}
	}
	p.mu.Unlock()
	for _, tc := range kill {
		tc.src.Close()
		tc.up.Close()
	}
}

func (p *fakeProxy) exitHooks() (func(string) int, func(string) (string, bool), func(string) (string, bool)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.markFor, p.dcAddrFor, p.dnsAddrFor
}

func (p *fakeProxy) Start(bindIP string, port int) error {
	ip := net.ParseIP(bindIP)
	if ip == nil {
		return fmt.Errorf("invalid fake proxy bind address %q", bindIP)
	}
	ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: ip, Port: port})
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.ln = ln
	p.mu.Unlock()
	p.wg.Add(1)
	go p.serve(ln)
	return nil
}

func (p *fakeProxy) serve(ln *net.TCPListener) {
	defer p.wg.Done()
	for {
		conn, err := ln.AcceptTCP()
		if err != nil {
			p.mu.Lock()
			closed := p.closed
			p.mu.Unlock()
			if closed {
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.handle(conn)
		}()
	}
}

func (p *fakeProxy) handle(src *net.TCPConn) {
	defer src.Close()
	dst, err := originalDst(src)
	if err != nil {
		log.Printf("fakeproxy: SO_ORIGINAL_DST: %v", err)
		return
	}
	pol := p.curPolicy()
	if pol == nil {
		log.Printf("fakeproxy: %s arrived before policy was set, refusing", src.RemoteAddr())
		return
	}
	domain, ok := pol.LookupByFakeIP(dst.IP)
	if !ok {
		log.Printf("fakeproxy: %s not in fake-ip pool (expired?), dropping", dst.IP)
		return
	}
	vmName := ""
	if p.vmByIP != nil {
		vmName = p.vmByIP(src.RemoteAddr().(*net.TCPAddr).IP)
	}
	dec := pol.DecideFlow(domain, dst.IP, vmName)
	switch dec.Route {
	case gwpolicy.Direct:
		p.dialDirect(src, domain, dst.Port, vmName)
	default:
		p.dialProxy(src, domain, dst.Port, dec.Exit, vmName)
	}
}

func (p *fakeProxy) track(tc *trackedConn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.policy == nil {
		return false
	}
	dec := p.policy.DecideFlow(tc.domain, nil, tc.vm)
	if dec.Route != tc.route || dec.Exit != tc.exit || (tc.route == gwpolicy.Proxy && p.markFor(tc.exit) < 0) {
		return false
	}
	p.conns[tc] = struct{}{}
	return true
}

func (p *fakeProxy) untrack(tc *trackedConn) {
	p.mu.Lock()
	delete(p.conns, tc)
	p.mu.Unlock()
}

func (p *fakeProxy) dialDirect(src *net.TCPConn, domain string, port int, vmName string) {
	if domain == probeDomain {
		log.Printf("fakeproxy: reserved probe has no direct target")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	ips, err := p.resolver.LookupIP(ctx, "ip4", domain)
	cancel()
	if err != nil || len(ips) == 0 {
		log.Printf("fakeproxy: resolve %s: %v", domain, err)
		return
	}
	up, err := net.DialTimeout("tcp4", net.JoinHostPort(ips[0].String(), fmt.Sprint(port)), 10*time.Second)
	if err != nil {
		log.Printf("fakeproxy: dial %s (%s:%d): %v", domain, ips[0], port, err)
		return
	}
	defer up.Close()
	tc := &trackedConn{vm: vmName, domain: domain, route: gwpolicy.Direct, src: src, up: up}
	if !p.track(tc) {
		return
	}
	defer p.untrack(tc)
	relay(src, up)
}

func (p *fakeProxy) dialProxy(src *net.TCPConn, domain string, port int, exitName string, vmName string) {
	markFor, dcAddrFor, dnsAddrFor := p.exitHooks()
	mark := markFor(exitName)
	if mark < 0 {
		log.Printf("fakeproxy: %s wants exit %q but tunnel is down; refusing fast", domain, exitName)
		return
	}
	if dcAddr, ok := dcAddrFor(exitName); ok {
		d := net.Dialer{Timeout: 10 * time.Second, Control: soMark(mark)}
		up, err := d.DialContext(context.Background(), "tcp4", dcAddr)
		if err != nil {
			log.Printf("fakeproxy: domain-connect %s via %s (%s): %v", domain, exitName, dcAddr, err)
			return
		}
		if err := exit.WriteDomainConnect(up, domain, port); err != nil {
			log.Printf("fakeproxy: domain-egress %s:%d via %s: %v", domain, port, exitName, err)
			up.Close()
			return
		}
		p.relayProxy(src, up, domain, exitName, vmName)
		return
	}
	dnsAddr, ok := dnsAddrFor(exitName)
	if !ok {
		log.Printf("fakeproxy: %s wants exit %q but neither domain-connect nor tunnel DNS is available; refusing fast", domain, exitName)
		return
	}
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		d := net.Dialer{Timeout: 5 * time.Second, Control: soMark(mark)}
		return d.DialContext(ctx, "udp", dnsAddr)
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	ips, err := resolver.LookupIP(ctx, "ip4", domain)
	cancel()
	if err != nil || len(ips) == 0 {
		log.Printf("fakeproxy: resolve %s via %s (%s): %v", domain, exitName, dnsAddr, err)
		return
	}
	d := net.Dialer{Timeout: 10 * time.Second, Control: soMark(mark)}
	up, err := d.DialContext(context.Background(), "tcp4", net.JoinHostPort(ips[0].String(), fmt.Sprint(port)))
	if err != nil {
		log.Printf("fakeproxy: dial %s (%s:%d) via %s: %v", domain, ips[0], port, exitName, err)
		return
	}
	p.relayProxy(src, up, domain, exitName, vmName)
}

func (p *fakeProxy) relayProxy(src *net.TCPConn, up net.Conn, domain, exitName, vmName string) {
	defer up.Close()
	tc := &trackedConn{vm: vmName, domain: domain, route: gwpolicy.Proxy, exit: exitName, src: src, up: up}
	if !p.track(tc) {
		return
	}
	defer p.untrack(tc)
	relay(src, up)
}

func relay(a, b net.Conn) {
	if tc, ok := a.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	if tc, ok := b.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	done := make(chan struct{}, 2)
	cp := func(to, from net.Conn) {
		copyWithIdleTimeout(to, from, relayIdleTimeout)
		if tc, ok := from.(*net.TCPConn); ok {
			tc.CloseRead()
		}
		if tc, ok := to.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
}

// soMarkOpt is SO_MARK on linux (x/sys/unix does not export it on
// darwin; this code runs in the linux gateway guest but compiles on the
// host too).
const soMarkOpt = 36

// soMark returns a net.Dialer Control hook setting SO_MARK, which the
// gateway's policy routing (ip rule fwmark) sends into the exit tunnel.
func soMark(mark int) func(network, address string, c syscall.RawConn) error {
	if mark == 0 {
		return nil
	}
	return func(network, address string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, soMarkOpt, mark)
		})
		if err != nil {
			return err
		}
		return serr
	}
}

func (p *fakeProxy) Close() {
	p.mu.Lock()
	p.closed = true
	ln := p.ln
	for tc := range p.conns {
		tc.src.Close()
		tc.up.Close()
	}
	p.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
	p.wg.Wait()
}

const soOriginalDst = 80 // SO_ORIGINAL_DST, linux netfilter

// originalDst recovers the NAT destination of a REDIRECTed connection:
// the fake IP the client actually dialed.
func originalDst(conn *net.TCPConn) (*net.TCPAddr, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return nil, err
	}
	var addr *net.TCPAddr
	var serr error
	if err := raw.Control(func(fd uintptr) {
		var mreq [16]byte
		size := uint32(len(mreq))
		// getsockopt(SOL_IP, SO_ORIGINAL_DST) -> sockaddr_in
		_, _, errno := syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd, 0, soOriginalDst,
			uintptr(unsafe.Pointer(&mreq[0])), uintptr(unsafe.Pointer(&size)), 0)
		if errno != 0 {
			serr = errno
			return
		}
		port := int(mreq[2])<<8 | int(mreq[3])
		addr = &net.TCPAddr{IP: net.IPv4(mreq[4], mreq[5], mreq[6], mreq[7]), Port: port}
	}); err != nil {
		return nil, err
	}
	if serr != nil {
		return nil, serr
	}
	return addr, nil
}

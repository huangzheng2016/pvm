package gateway

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	gwpolicy "github.com/huangzheng2016/pvm/internal/gw/policy"
)

// fakeProxy terminates TCP connections the nft rules REDIRECT from the
// fake-ip range: the original destination (a fake IP) is recovered with
// SO_ORIGINAL_DST, mapped back to its domain, routed by policy and
// dialed out. Direct flows are resolved via the uplink resolver and
// dialed from the gateway; proxied flows carry an SO_MARK that policy
// routing sends into the exit tunnel, and their DNS is resolved through
// the tunnel at a public resolver (remote-resolve semantics).
type fakeProxy struct {
	policy   *gwpolicy.Engine
	resolver *net.Resolver
	vmByIP   func(ip net.IP) string // config lookup, nil-safe
	markFor  func(exitName string) int // SO_MARK selecting the exit's table; -1 = no tunnel

	mu       sync.Mutex
	ln       *net.TCPListener
	closed   bool
	wg       sync.WaitGroup
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
		vmByIP: vmByIP,
		markFor: func(string) int { return -1 },
	}
}

// SetPolicy swaps the flow policy on config reload.
func (p *fakeProxy) SetPolicy(eng *gwpolicy.Engine) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.policy = eng
}

func (p *fakeProxy) curPolicy() *gwpolicy.Engine {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.policy
}

func (p *fakeProxy) Start(port int) error {
	ln, err := net.ListenTCP("tcp4", &net.TCPAddr{Port: port})
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
		p.dialDirect(src, domain, dst.Port)
	default:
		p.dialProxy(src, domain, dst.Port, dec.Exit)
	}
}

func (p *fakeProxy) dialDirect(src *net.TCPConn, domain string, port int) {
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
	relay(src, up)
}

// dialProxy carries the flow out via the named exit tunnel. Both the
// DNS lookup and the target dial are marked, so policy routing sends
// them through the tunnel: the gateway never resolves proxied domains
// on the local uplink (remote-resolve semantics).
func (p *fakeProxy) dialProxy(src *net.TCPConn, domain string, port int, exitName string) {
	mark := p.markFor(exitName)
	if mark < 0 {
		log.Printf("fakeproxy: %s wants exit %q but tunnel is down; refusing fast", domain, exitName)
		return
	}
	markedDial := func(ctx context.Context, network, address string) (net.Conn, error) {
		d := net.Dialer{Timeout: 8 * time.Second, Control: soMark(mark)}
		return d.DialContext(ctx, network, address)
	}
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			return markedDial(ctx, "udp", "1.1.1.1:53")
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	ips, err := resolver.LookupIP(ctx, "ip4", domain)
	cancel()
	if err != nil || len(ips) == 0 {
		log.Printf("fakeproxy: remote-resolve %s via %s: %v", domain, exitName, err)
		return
	}
	up, err := markedDial(context.Background(), "tcp4", net.JoinHostPort(ips[0].String(), fmt.Sprint(port)))
	if err != nil {
		log.Printf("fakeproxy: dial %s (%s:%d) via %s: %v", domain, ips[0], port, exitName, err)
		return
	}
	defer up.Close()
	relay(src, up)
}

// relay shuttles bytes both ways, propagating half-closes.
func relay(a, b net.Conn) {
	if tc, ok := a.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	if tc, ok := b.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	done := make(chan struct{}, 2)
	cp := func(to, from net.Conn) {
		io.Copy(to, from)
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

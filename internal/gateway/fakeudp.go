package gateway

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/huangzheng2016/pvm/internal/exit"
	gwpolicy "github.com/huangzheng2016/pvm/internal/gw/policy"
	"golang.org/x/sys/unix"
)

const (
	fakeUDPPort        = 7894
	fakeUDPMaxFlows    = 4096
	fakeUDPIdleTimeout = 2 * time.Minute
)

type fakeUDPKey struct {
	srcIP   string
	srcPort int
	dstIP   string
	dstPort int
}

type fakeUDPFlow struct {
	key        fakeUDPKey
	src        *net.UDPAddr
	dst        *net.UDPAddr
	domain     string
	vm         string
	route      gwpolicy.Route
	exit       string
	up         *net.UDPConn
	down       *net.IPConn
	lastActive time.Time
}

func (f *fakeUDPFlow) close() {
	f.up.Close()
	if f.down != nil {
		f.down.Close()
	}
}

type fakeUDP struct {
	policy     *gwpolicy.Engine
	resolver   *net.Resolver
	vmByIP     func(ip net.IP) string
	markFor    func(exitName string) int
	dcAddrFor  func(exitName string) (string, bool)
	dnsAddrFor func(exitName string) (string, bool)

	lookupDirect func(context.Context, string) (net.IP, error)
	lookupProxy  func(context.Context, string, string, int) (net.IP, error)

	mu         sync.Mutex
	ln         *net.UDPConn
	rawReplies bool
	closed     bool
	wg         sync.WaitGroup
	flows      map[fakeUDPKey]*fakeUDPFlow
	pending    map[fakeUDPKey]bool
}

func newFakeUDP(policy *gwpolicy.Engine, upstream string, vmByIP func(net.IP) string) *fakeUDP {
	return &fakeUDP{
		policy: policy,
		resolver: &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, "udp", upstream+":53")
		}},
		vmByIP:     vmByIP,
		markFor:    func(string) int { return -1 },
		dcAddrFor:  func(string) (string, bool) { return "", false },
		dnsAddrFor: func(string) (string, bool) { return "", false },
		flows:      map[fakeUDPKey]*fakeUDPFlow{},
		pending:    map[fakeUDPKey]bool{},
	}
}

func (p *fakeUDP) Start(bindIP string, port int) error {
	ip := net.ParseIP(bindIP)
	if ip == nil {
		return fmt.Errorf("invalid UDP relay bind address %q", bindIP)
	}
	ln, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip, Port: port})
	if err != nil {
		return err
	}
	if err := enableUDPTransparentSocket(ln); err != nil {
		ln.Close()
		return err
	}
	if err := enableUDPOriginalDst(ln); err != nil {
		ln.Close()
		return err
	}
	p.mu.Lock()
	p.ln = ln
	p.rawReplies = true
	p.wg.Add(2)
	p.mu.Unlock()
	go p.readLoop(ln)
	go p.reaper()
	return nil
}

const ipTransparent = 19

func setSocketTransparent(c syscall.RawConn) error {
	var sockErr error
	if err := c.Control(func(fd uintptr) {
		sockErr = unix.SetsockoptInt(int(fd), 0, ipTransparent, 1)
	}); err != nil {
		return err
	}
	return sockErr
}

func transparentSocket(_, _ string, c syscall.RawConn) error {
	return setSocketTransparent(c)
}

func enableUDPTransparentSocket(conn *net.UDPConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	return setSocketTransparent(raw)
}

func dialUDPReply(originalDst, client *net.UDPAddr) (*net.IPConn, error) {
	d := net.Dialer{
		Timeout:   5 * time.Second,
		LocalAddr: &net.IPAddr{IP: originalDst.IP},
		Control:   transparentSocket,
	}
	conn, err := d.Dial("ip4:udp", client.IP.String())
	if err != nil {
		return nil, err
	}
	return conn.(*net.IPConn), nil
}

func (p *fakeUDP) SetPolicy(eng *gwpolicy.Engine) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.policy = eng
	for key, flow := range p.flows {
		dec := eng.DecideFlow(flow.domain, net.ParseIP(key.dstIP), flow.vm)
		if dec.Route != flow.route || dec.Exit != flow.exit {
			delete(p.flows, key)
			flow.close()
		}
	}
}

func (p *fakeUDP) SetExitHooks(markFor func(string) int, dcAddrFor, dnsAddrFor func(string) (string, bool)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.markFor, p.dcAddrFor, p.dnsAddrFor = markFor, dcAddrFor, dnsAddrFor
	for key, flow := range p.flows {
		if flow.route == gwpolicy.Proxy && markFor(flow.exit) < 0 {
			delete(p.flows, key)
			flow.close()
		}
	}
}

func (p *fakeUDP) curPolicy() *gwpolicy.Engine {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.policy
}

func (p *fakeUDP) exitHooks() (func(string) int, func(string) (string, bool), func(string) (string, bool)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.markFor, p.dcAddrFor, p.dnsAddrFor
}

func (p *fakeUDP) readLoop(ln *net.UDPConn) {
	defer p.wg.Done()
	buf := make([]byte, 64*1024)
	oob := make([]byte, 128)
	for {
		n, oobn, _, src, err := ln.ReadMsgUDP(buf, oob)
		if err != nil {
			if p.isClosed() {
				return
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		dst, err := originalDstUDP(oob[:oobn])
		if err != nil {
			log.Printf("fakeudp: original destination: %v", err)
			continue
		}
		pol := p.curPolicy()
		if pol == nil {
			continue
		}
		domain, ok := pol.LookupByFakeIP(dst.IP)
		if !ok {
			continue
		}
		key := fakeUDPKey{srcIP: src.IP.String(), srcPort: src.Port, dstIP: dst.IP.String(), dstPort: dst.Port}
		payload := append([]byte(nil), buf[:n]...)
		flow, start := p.reserve(key)
		if flow != nil {
			if _, err := flow.up.Write(payload); err != nil {
				p.closeKey(key)
			}
			continue
		}
		if start {
			go p.openFlow(key, src, dst, domain, payload)
		}
	}
}

func (p *fakeUDP) reserve(key fakeUDPKey) (*fakeUDPFlow, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if flow := p.flows[key]; flow != nil {
		flow.lastActive = time.Now()
		return flow, false
	}
	if p.closed || p.pending[key] || len(p.flows)+len(p.pending) >= fakeUDPMaxFlows {
		return nil, false
	}
	p.pending[key] = true
	p.wg.Add(1)
	return nil, true
}

func (p *fakeUDP) openFlow(key fakeUDPKey, src, dst *net.UDPAddr, domain string, payload []byte) {
	defer p.wg.Done()
	vmName := ""
	if p.vmByIP != nil {
		vmName = p.vmByIP(src.IP)
	}
	up, route, exitName, err := p.dial(domain, dst, vmName)
	if err != nil {
		p.mu.Lock()
		delete(p.pending, key)
		p.mu.Unlock()
		log.Printf("fakeudp: open %s via %s: %v", domain, exitName, err)
		return
	}
	p.mu.Lock()
	rawReplies := p.rawReplies
	p.mu.Unlock()
	var down *net.IPConn
	if rawReplies {
		down, err = dialUDPReply(dst, src)
		if err != nil {
			up.Close()
			p.mu.Lock()
			delete(p.pending, key)
			p.mu.Unlock()
			log.Printf("fakeudp: open %s via %s: reply path: %v", domain, exitName, err)
			return
		}
	}
	p.mu.Lock()
	delete(p.pending, key)
	adopt := false
	if !p.closed && p.policy != nil {
		dec := p.policy.DecideFlow(domain, dst.IP, vmName)
		if dec.Route == route && dec.Exit == exitName && (route != gwpolicy.Proxy || p.markFor(exitName) >= 0) {
			flow := &fakeUDPFlow{key: key, src: src, dst: dst, domain: domain, vm: vmName, route: route, exit: exitName, up: up, down: down, lastActive: time.Now()}
			p.flows[key] = flow
			p.wg.Add(1)
			go p.pump(flow)
			adopt = true
		}
	}
	p.mu.Unlock()
	if !adopt {
		up.Close()
		if down != nil {
			down.Close()
		}
		return
	}
	if _, err := up.Write(payload); err != nil {
		p.closeKey(key)
	}
}

func (p *fakeUDP) dial(domain string, dst *net.UDPAddr, vmName string) (*net.UDPConn, gwpolicy.Route, string, error) {
	pol := p.curPolicy()
	if pol == nil {
		return nil, gwpolicy.Direct, "", fmt.Errorf("policy is not ready")
	}
	dec := pol.DecideFlow(domain, dst.IP, vmName)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if dec.Route == gwpolicy.Direct {
		lookup := p.lookupDirect
		if lookup == nil {
			lookup = p.resolveDirect
		}
		ip, err := lookup(ctx, domain)
		if err != nil {
			return nil, dec.Route, dec.Exit, err
		}
		up, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: ip, Port: dst.Port})
		return up, dec.Route, dec.Exit, err
	}
	markFor, dcAddrFor, dnsAddrFor := p.exitHooks()
	mark := markFor(dec.Exit)
	if mark < 0 {
		return nil, dec.Route, dec.Exit, fmt.Errorf("tunnel is down")
	}
	lookup := p.lookupProxy
	if lookup == nil {
		lookup = func(ctx context.Context, domain, exitName string, mark int) (net.IP, error) {
			return p.resolveProxy(ctx, domain, exitName, mark, dcAddrFor, dnsAddrFor)
		}
	}
	ip, err := lookup(ctx, domain, dec.Exit, mark)
	if err != nil {
		return nil, dec.Route, dec.Exit, err
	}
	d := net.Dialer{Timeout: 10 * time.Second, Control: soMark(mark)}
	conn, err := d.DialContext(ctx, "udp4", net.JoinHostPort(ip.String(), fmt.Sprint(dst.Port)))
	if err != nil {
		return nil, dec.Route, dec.Exit, err
	}
	return conn.(*net.UDPConn), dec.Route, dec.Exit, nil
}

func (p *fakeUDP) resolveDirect(ctx context.Context, domain string) (net.IP, error) {
	if domain == probeDomain {
		return nil, fmt.Errorf("reserved probe has no direct target")
	}
	ips, err := p.resolver.LookupIP(ctx, "ip4", domain)
	return firstIPv4(ips, err)
}

const probeDomain = "pvm.probe"

func probeAddress(domain, exitName string, dcAddrFor func(string) (string, bool)) (net.IP, error) {
	if domain != probeDomain {
		return nil, nil
	}
	addr, ok := dcAddrFor(exitName)
	if !ok {
		return nil, fmt.Errorf("no self-hosted probe for %s", exitName)
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("invalid probe address %q", addr)
	}
	return ip, nil
}

func (p *fakeUDP) resolveProxy(ctx context.Context, domain, exitName string, mark int, dcAddrFor, dnsAddrFor func(string) (string, bool)) (net.IP, error) {
	if ip, err := probeAddress(domain, exitName, dcAddrFor); ip != nil || err != nil {
		return ip, err
	}
	if dcAddr, ok := dcAddrFor(exitName); ok {
		d := net.Dialer{Timeout: 10 * time.Second, Control: soMark(mark)}
		conn, err := d.DialContext(ctx, "tcp4", dcAddr)
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		return exit.WriteDomainResolve(conn, domain)
	}
	if dnsAddr, ok := dnsAddrFor(exitName); ok {
		resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second, Control: soMark(mark)}
			return d.DialContext(ctx, "udp", dnsAddr)
		}}
		ips, err := resolver.LookupIP(ctx, "ip4", domain)
		return firstIPv4(ips, err)
	}
	return nil, fmt.Errorf("no in-tunnel resolver for %s", exitName)
}

func firstIPv4(ips []net.IP, err error) (net.IP, error) {
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			return v4, nil
		}
	}
	return nil, fmt.Errorf("no IPv4 address")
}

func (p *fakeUDP) pump(flow *fakeUDPFlow) {
	defer p.wg.Done()
	buf := make([]byte, 64*1024)
	for {
		n, err := flow.up.Read(buf)
		if err != nil {
			p.closeKey(flow.key)
			return
		}
		p.mu.Lock()
		if p.closed || p.flows[flow.key] != flow {
			p.mu.Unlock()
			return
		}
		flow.lastActive = time.Now()
		ln := p.ln
		p.mu.Unlock()
		if flow.down != nil {
			if err := writeUDPReply(flow.down, flow.dst, flow.src, buf[:n]); err != nil {
				p.closeKey(flow.key)
				return
			}
		} else if ln != nil {
			if _, err := ln.WriteToUDP(buf[:n], flow.src); err != nil {
				p.closeKey(flow.key)
				return
			}
		}
	}
}

func writeUDPReply(conn *net.IPConn, originalDst, client *net.UDPAddr, payload []byte) error {
	packet := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint16(packet[0:2], uint16(originalDst.Port))
	binary.BigEndian.PutUint16(packet[2:4], uint16(client.Port))
	binary.BigEndian.PutUint16(packet[4:6], uint16(len(packet)))
	copy(packet[8:], payload)
	binary.BigEndian.PutUint16(packet[6:8], udpChecksum(originalDst.IP, client.IP, packet))
	_, err := conn.Write(packet)
	return err
}

func udpChecksum(src, dst net.IP, packet []byte) uint16 {
	var sum uint32
	for _, ip := range []net.IP{src.To4(), dst.To4()} {
		for i := 0; i < net.IPv4len; i += 2 {
			sum += uint32(ip[i])<<8 | uint32(ip[i+1])
		}
	}
	sum += uint32(17) + uint32(len(packet))
	for i := 0; i < len(packet); i += 2 {
		sum += uint32(packet[i]) << 8
		if i+1 < len(packet) {
			sum += uint32(packet[i+1])
		}
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	checksum := ^uint16(sum)
	if checksum == 0 {
		return 0xffff
	}
	return checksum
}

func (p *fakeUDP) closeKey(key fakeUDPKey) {
	p.mu.Lock()
	flow := p.flows[key]
	delete(p.flows, key)
	p.mu.Unlock()
	if flow != nil {
		flow.close()
	}
}

func (p *fakeUDP) reaper() {
	defer p.wg.Done()
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			p.mu.Lock()
			if p.closed {
				p.mu.Unlock()
				return
			}
			for key, flow := range p.flows {
				if time.Since(flow.lastActive) > fakeUDPIdleTimeout {
					delete(p.flows, key)
					flow.close()
				}
			}
			p.mu.Unlock()
		default:
			if p.isClosed() {
				return
			}
			time.Sleep(time.Second)
		}
	}
}

func (p *fakeUDP) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

func (p *fakeUDP) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	if p.ln != nil {
		p.ln.Close()
	}
	for _, flow := range p.flows {
		flow.close()
	}
	p.mu.Unlock()
	p.wg.Wait()
}

const (
	ipRecvOriginalDst = 20
	ipOriginalDst     = 20
)

func enableUDPOriginalDst(conn *net.UDPConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		sockErr = unix.SetsockoptInt(int(fd), 0, ipRecvOriginalDst, 1)
	}); err != nil {
		return err
	}
	return sockErr
}

func originalDstUDP(oob []byte) (*net.UDPAddr, error) {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, err
	}
	for i := range msgs {
		m := &msgs[i]
		if int(m.Header.Level) == 0 && int(m.Header.Type) == ipOriginalDst && len(m.Data) >= 16 {
			return &net.UDPAddr{
				IP:   net.IPv4(m.Data[4], m.Data[5], m.Data[6], m.Data[7]),
				Port: int(m.Data[2])<<8 | int(m.Data[3]),
			}, nil
		}
	}
	return nil, fmt.Errorf("original destination control message not found")
}

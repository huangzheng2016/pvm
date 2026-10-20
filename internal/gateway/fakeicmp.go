package gateway

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/huangzheng2016/pvm/internal/exit"
	gwpolicy "github.com/huangzheng2016/pvm/internal/gw/policy"
	"golang.org/x/sys/unix"
)

const (
	fakeICMPMaxFlows    = 1024
	fakeICMPIdleTimeout = 2 * time.Minute

	icmpEchoReply   = 0
	icmpEchoRequest = 8
)

// fakeICMP relays ICMP echo requests the nft rules DNAT from the fake-ip
// range to the gateway: the original destination (a fake IP) is recovered
// with IP_RECVORIGDSTADDR, mapped back to its domain, routed by policy
// and echoed to the real target over a per-flow raw socket. Direct flows
// (including internal-rule destinations) resolve via the uplink resolver;
// proxied flows resolve through the exit tunnel only, so a tunnel or
// resolve failure drops the flow instead of leaking it direct. Replies
// leave through the same raw socket, letting conntrack reverse the DNAT.
type fakeICMP struct {
	policy     *gwpolicy.Engine
	resolver   *net.Resolver
	vmByIP     func(ip net.IP) string
	markFor    func(exitName string) int
	dcAddrFor  func(exitName string) (string, bool)
	dnsAddrFor func(exitName string) (string, bool)

	lookupDirect func(context.Context, string) (net.IP, error)
	lookupProxy  func(context.Context, string, string, int) (net.IP, error)
	dialICMP     func(ctx context.Context, address string, mark int) (net.Conn, error)
	dialDC       func(ctx context.Context, address string, mark int) (net.Conn, error)

	mu       sync.Mutex
	ln       icmpPacketConn
	closed   bool
	wg       sync.WaitGroup
	nextUpID uint32
	flows    map[fakeICMPKey]*fakeICMPFlow
	pending  map[fakeICMPKey]bool
}

// icmpPacketConn is the subset of *net.IPConn the relay needs; tests swap
// in a fake so no raw socket or netfilter setup is required.
type icmpPacketConn interface {
	ReadMsgIP(b, oob []byte) (n, oobn, flags int, addr *net.IPAddr, err error)
	WriteToIP(b []byte, addr *net.IPAddr) (int, error)
	Close() error
}

// fakeICMPKey identifies one echo session; ICMP has no ports, so the
// identifier takes their place together with both addresses.
type fakeICMPKey struct {
	srcIP string
	dstIP string
	id    int
}

type fakeICMPFlow struct {
	key        fakeICMPKey
	src        *net.IPAddr
	domain     string
	vm         string
	route      gwpolicy.Route
	exit       string
	origID     int // identifier the VM used, restored on the reply
	upID       int // identifier assigned by the upstream socket
	up         net.Conn
	lastActive time.Time
}

func newFakeICMP(policy *gwpolicy.Engine, upstream string, vmByIP func(net.IP) string) *fakeICMP {
	return &fakeICMP{
		policy: policy,
		resolver: &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, "udp", upstream+":53")
		}},
		vmByIP:     vmByIP,
		markFor:    func(string) int { return -1 },
		dcAddrFor:  func(string) (string, bool) { return "", false },
		dnsAddrFor: func(string) (string, bool) { return "", false },
		flows:      map[fakeICMPKey]*fakeICMPFlow{},
		pending:    map[fakeICMPKey]bool{},
	}
}

func (p *fakeICMP) Start(bindIP string) error {
	ip := net.ParseIP(bindIP)
	if ip == nil {
		return fmt.Errorf("invalid ICMP relay bind address %q", bindIP)
	}
	ln, err := net.ListenIP("ip4:icmp", &net.IPAddr{IP: ip})
	if err != nil {
		return err
	}
	if err := enableIPOriginalDst(ln); err != nil {
		ln.Close()
		return err
	}
	p.mu.Lock()
	p.ln = ln
	p.wg.Add(2)
	p.mu.Unlock()
	go p.readLoop(ln)
	go p.reaper()
	return nil
}

// SetPolicy swaps the flow policy on config reload and kills every live
// flow whose decision flipped, so the next echo re-resolves under the
// new route instead of leaking on the old one.
func (p *fakeICMP) SetPolicy(eng *gwpolicy.Engine) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.policy = eng
	for key, flow := range p.flows {
		dec := eng.DecideFlow(flow.domain, net.ParseIP(key.dstIP), flow.vm)
		if dec.Route != flow.route || dec.Exit != flow.exit {
			delete(p.flows, key)
			flow.up.Close()
		}
	}
}

// SetExitHooks installs the tunnel lookup hooks on config reload and kills
// proxied flows whose tunnel is now down: fail closed, never direct.
func (p *fakeICMP) SetExitHooks(markFor func(string) int, dcAddrFor, dnsAddrFor func(string) (string, bool)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.markFor, p.dcAddrFor, p.dnsAddrFor = markFor, dcAddrFor, dnsAddrFor
	for key, flow := range p.flows {
		if flow.route == gwpolicy.Proxy && markFor(flow.exit) < 0 {
			delete(p.flows, key)
			flow.up.Close()
		}
	}
}

func (p *fakeICMP) curPolicy() *gwpolicy.Engine {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.policy
}

func (p *fakeICMP) exitHooks() (func(string) int, func(string) (string, bool), func(string) (string, bool)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.markFor, p.dcAddrFor, p.dnsAddrFor
}

func (p *fakeICMP) readLoop(ln icmpPacketConn) {
	defer p.wg.Done()
	buf := make([]byte, 64*1024)
	oob := make([]byte, 128)
	for {
		n, oobn, _, src, err := ln.ReadMsgIP(buf, oob)
		if err != nil {
			if p.isClosed() {
				return
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		dst, err := originalDstUDP(oob[:oobn])
		if err != nil {
			log.Printf("fakeicmp: original destination: %v", err)
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
		msg := icmpMessage(buf[:n])
		id, ok := parseEchoRequest(msg)
		if !ok {
			continue
		}
		srcAddr := src
		if srcAddr == nil || p.isUpstreamID(id) {
			continue
		}
		key := fakeICMPKey{srcIP: srcAddr.IP.String(), dstIP: dst.IP.String(), id: id}
		payload := append([]byte(nil), msg...)
		flow, start := p.reserve(key)
		if flow != nil {
			if err := p.writeUpstream(flow, payload); err != nil {
				p.closeKey(key)
			}
			continue
		}
		if start {
			go p.openFlow(key, srcAddr, dst.IP, domain, payload)
		}
	}
}

func (p *fakeICMP) reserve(key fakeICMPKey) (*fakeICMPFlow, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if flow := p.flows[key]; flow != nil {
		flow.lastActive = time.Now()
		return flow, false
	}
	if p.closed || p.pending[key] || len(p.flows)+len(p.pending) >= fakeICMPMaxFlows {
		return nil, false
	}
	p.pending[key] = true
	p.wg.Add(1)
	return nil, true
}

func (p *fakeICMP) isUpstreamID(id int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, flow := range p.flows {
		if flow.upID == id {
			return true
		}
	}
	return false
}

func (p *fakeICMP) openFlow(key fakeICMPKey, src *net.IPAddr, dstIP net.IP, domain string, payload []byte) {
	defer p.wg.Done()
	vmName := ""
	if p.vmByIP != nil {
		vmName = p.vmByIP(src.IP)
	}
	up, route, exitName, err := p.dial(domain, dstIP, vmName)
	if err != nil {
		p.mu.Lock()
		delete(p.pending, key)
		p.mu.Unlock()
		log.Printf("fakeicmp: open %s via %s: %v", domain, exitName, err)
		return
	}
	p.mu.Lock()
	delete(p.pending, key)
	var flow *fakeICMPFlow
	if !p.closed && p.policy != nil {
		dec := p.policy.DecideFlow(domain, dstIP, vmName)
		if dec.Route == route && dec.Exit == exitName && (route != gwpolicy.Proxy || p.markFor(exitName) >= 0) {
			flow = &fakeICMPFlow{key: key, src: src, domain: domain, vm: vmName, route: route, exit: exitName, origID: key.id, upID: p.allocUpID(), up: up, lastActive: time.Now()}
			p.flows[key] = flow
			p.wg.Add(1)
			go p.pump(flow)
		}
	}
	p.mu.Unlock()
	if flow == nil {
		up.Close()
		return
	}
	if err := p.writeUpstream(flow, payload); err != nil {
		p.closeKey(key)
	}
}

// dial resolves the target and opens the per-flow raw ICMP socket. The
// returned route/exit let the caller re-check the decision before adopting.
func (p *fakeICMP) dial(domain string, dstIP net.IP, vmName string) (net.Conn, gwpolicy.Route, string, error) {
	pol := p.curPolicy()
	if pol == nil {
		return nil, gwpolicy.Direct, "", fmt.Errorf("policy is not ready")
	}
	dec := pol.DecideFlow(domain, dstIP, vmName)
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
		up, err := p.dialRaw(ctx, ip.String(), -1)
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
	up, err := p.dialRaw(ctx, ip.String(), mark)
	return up, dec.Route, dec.Exit, err
}

// dialRaw opens the outbound raw ICMP socket; mark >= 0 tags it for the
// exit's policy-routing table. Replaceable in tests.
func (p *fakeICMP) dialRaw(ctx context.Context, address string, mark int) (net.Conn, error) {
	if p.dialICMP != nil {
		return p.dialICMP(ctx, address, mark)
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	if mark >= 0 {
		d.Control = soMark(mark)
	}
	return d.DialContext(ctx, "ip4:icmp", address)
}

// dialDCRaw opens the TCP connection to the exit's domain-connect
// service. Replaceable in tests.
func (p *fakeICMP) dialDCRaw(ctx context.Context, address string, mark int) (net.Conn, error) {
	if p.dialDC != nil {
		return p.dialDC(ctx, address, mark)
	}
	d := net.Dialer{Timeout: 10 * time.Second, Control: soMark(mark)}
	return d.DialContext(ctx, "tcp4", address)
}

func (p *fakeICMP) resolveDirect(ctx context.Context, domain string) (net.IP, error) {
	ips, err := p.resolver.LookupIP(ctx, "ip4", domain)
	return firstIPv4(ips, err)
}

// resolveProxy resolves through the tunnel only: the exit-agent's
// domain-connect service when available, else the exit's in-tunnel DNS.
func (p *fakeICMP) resolveProxy(ctx context.Context, domain, exitName string, mark int, dcAddrFor, dnsAddrFor func(string) (string, bool)) (net.IP, error) {
	if dcAddr, ok := dcAddrFor(exitName); ok {
		conn, err := p.dialDCRaw(ctx, dcAddr, mark)
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

// writeUpstream rewrites the echo identifier to the flow's upstream one
// and fixes the checksum before handing the message to the raw socket.
func (p *fakeICMP) writeUpstream(flow *fakeICMPFlow, payload []byte) error {
	msg := append([]byte(nil), payload...)
	binary.BigEndian.PutUint16(msg[4:6], uint16(flow.upID))
	icmpSetChecksum(msg)
	_, err := flow.up.Write(msg)
	return err
}

// pump forwards echo replies from the raw socket back to the VM,
// restoring the VM's identifier so conntrack can reverse the DNAT.
func (p *fakeICMP) pump(flow *fakeICMPFlow) {
	defer p.wg.Done()
	buf := make([]byte, 64*1024)
	for {
		n, err := flow.up.Read(buf)
		if err != nil {
			p.closeKey(flow.key)
			return
		}
		msg := icmpMessage(buf[:n])
		if !isEchoReply(msg, flow.upID) {
			continue
		}
		p.mu.Lock()
		if p.closed || p.flows[flow.key] != flow {
			p.mu.Unlock()
			return
		}
		flow.lastActive = time.Now()
		ln := p.ln
		p.mu.Unlock()
		reply := append([]byte(nil), msg...)
		binary.BigEndian.PutUint16(reply[4:6], uint16(flow.origID))
		icmpSetChecksum(reply)
		if ln != nil {
			if _, err := ln.WriteToIP(reply, flow.src); err != nil {
				p.closeKey(flow.key)
				return
			}
		}
	}
}

func (p *fakeICMP) closeKey(key fakeICMPKey) {
	p.mu.Lock()
	flow := p.flows[key]
	delete(p.flows, key)
	p.mu.Unlock()
	if flow != nil {
		flow.up.Close()
	}
}

// reapIdle closes flows idle past fakeICMPIdleTimeout; their raw sockets
// go with them, so the next echo starts a fresh session.
func (p *fakeICMP) reapIdle() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	for key, flow := range p.flows {
		if time.Since(flow.lastActive) > fakeICMPIdleTimeout {
			delete(p.flows, key)
			flow.up.Close()
		}
	}
}

func (p *fakeICMP) reaper() {
	defer p.wg.Done()
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			p.reapIdle()
		default:
			if p.isClosed() {
				return
			}
			time.Sleep(time.Second)
		}
	}
}

func (p *fakeICMP) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

func (p *fakeICMP) Close() {
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
		flow.up.Close()
	}
	p.mu.Unlock()
	p.wg.Wait()
}

// allocUpID hands out the upstream identifier; unique per flow so two
// sessions to the same target never receive each other's replies. Called
// with p.mu held.
func (p *fakeICMP) allocUpID() int {
	p.nextUpID++
	if p.nextUpID > 0xffff {
		p.nextUpID = 1
	}
	return int(p.nextUpID)
}

func icmpMessage(packet []byte) []byte {
	if len(packet) >= 20 && packet[0]>>4 == 4 {
		hl := int(packet[0]&0x0f) * 4
		if hl >= 20 && hl <= len(packet) {
			return packet[hl:]
		}
	}
	return packet
}

func parseEchoRequest(msg []byte) (int, bool) {
	if len(msg) < 8 || msg[0] != icmpEchoRequest || msg[1] != 0 {
		return 0, false
	}
	return int(binary.BigEndian.Uint16(msg[4:6])), true
}

func isEchoReply(msg []byte, upID int) bool {
	return len(msg) >= 8 && msg[0] == icmpEchoReply && msg[1] == 0 &&
		int(binary.BigEndian.Uint16(msg[4:6])) == upID
}

// icmpSetChecksum recomputes the ICMP checksum over msg in place.
func icmpSetChecksum(msg []byte) {
	msg[2], msg[3] = 0, 0
	binary.BigEndian.PutUint16(msg[2:4], ^uint16(icmpSum(msg)))
}

func icmpSum(msg []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(msg); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(msg[i:]))
	}
	if len(msg)%2 == 1 {
		sum += uint32(msg[len(msg)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return uint16(sum)
}

// enableIPOriginalDst turns on IP_RECVORIGDSTADDR so ReadMsgIP reports
// the pre-DNAT destination for the DNATed echo requests.
func enableIPOriginalDst(conn *net.IPConn) error {
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

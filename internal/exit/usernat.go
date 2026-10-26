package exit

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/tun"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// userspaceBackend terminates WG-encrypted traffic in a gVisor stack and
// forwards flows with plain Go net.Dial — no OS TUN, no ip_forward, no
// netfilter, no root.
type userspaceBackend struct {
	stack    *stack.Stack
	sessions *udpSessionTable
	nicSeq   int
	mu       sync.Mutex
	pumps    []io.Closer
	cancel   context.CancelFunc
	ctx      context.Context
	stopped  bool
	tcpConns map[net.Conn]struct{}
	wg       sync.WaitGroup
	once     sync.Once

	icmpEp    tcpip.Endpoint
	icmpWq    *waiter.Queue
	icmpTable *icmpSessionTable
	// icmpDial is replaceable in tests; nil uses a real host ping socket.
	icmpDial func(network, address string) (net.Conn, error)

	intercept   map[string]func(net.Conn) // dst addr -> in-stack handler
	interceptMu sync.RWMutex

	interceptUDP map[string]func(*gonet.UDPConn) // dst addr -> in-stack datagram handler

	// icmpLocal lists tunnel-side .1 addresses this stack answers echo for
	// itself; no host ping socket or external route is involved.
	icmpLocal   map[string]bool
	icmpLocalMu sync.RWMutex
}

// Intercept registers an in-stack handler for connections to dst
// (host:port, e.g. a tunnel NIC's domain-connect address) instead of
// dialing that address from the VPS.
func (u *userspaceBackend) Intercept(dst string, h func(net.Conn)) {
	u.interceptMu.Lock()
	defer u.interceptMu.Unlock()
	if u.intercept == nil {
		u.intercept = map[string]func(net.Conn){}
	}
	u.intercept[dst] = h
}

func (u *userspaceBackend) interceptor(dst string) func(net.Conn) {
	u.interceptMu.RLock()
	defer u.interceptMu.RUnlock()
	return u.intercept[dst]
}

// InterceptUDP registers an in-stack datagram handler for UDP flows to dst
// (host:port, e.g. a tunnel NIC's probe address) instead of dialing that
// address from the VPS.
func (u *userspaceBackend) InterceptUDP(dst string, h func(*gonet.UDPConn)) {
	u.interceptMu.Lock()
	defer u.interceptMu.Unlock()
	if u.interceptUDP == nil {
		u.interceptUDP = map[string]func(*gonet.UDPConn){}
	}
	u.interceptUDP[dst] = h
}

func (u *userspaceBackend) udpInterceptor(dst string) func(*gonet.UDPConn) {
	u.interceptMu.RLock()
	defer u.interceptMu.RUnlock()
	return u.interceptUDP[dst]
}

// AnswerICMPLocal registers addr (a tunnel NIC's .1 address) as answered by
// the stack itself instead of relayed to a host ping socket.
func (u *userspaceBackend) AnswerICMPLocal(addr string) {
	u.icmpLocalMu.Lock()
	defer u.icmpLocalMu.Unlock()
	if u.icmpLocal == nil {
		u.icmpLocal = map[string]bool{}
	}
	u.icmpLocal[addr] = true
}

func (u *userspaceBackend) icmpIsLocal(addr string) bool {
	u.icmpLocalMu.RLock()
	defer u.icmpLocalMu.RUnlock()
	return u.icmpLocal[addr]
}

func newUserspaceBackend() *userspaceBackend {
	s := stack.New(stack.Options{
		NetworkProtocols: []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		// The ICMP transport protocol must be registered for the raw
		// endpoint (below) to attach; the stack itself never answers echo
		// because the NICs run in spoofing mode (temporary addresses).
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocolCUBIC, udp.NewProtocol, icmp.NewProtocol4},
	})
	ctx, cancel := context.WithCancel(context.Background())
	return &userspaceBackend{
		ctx: ctx, cancel: cancel, tcpConns: map[net.Conn]struct{}{},
		stack:    s,
		sessions: newUDPSessionTable(2 * time.Minute),
	}
}

func (*userspaceBackend) name() string { return NATUserspace }

// newTun returns a chanTUN bridged to a fresh NIC on our stack.
func (u *userspaceBackend) newTun(tunName string, mtu int) (tun.Device, error) {
	u.mu.Lock()
	u.nicSeq++
	nicID := tcpip.NICID(u.nicSeq)
	u.mu.Unlock()

	ct := newChanTUN(tunName, mtu)
	linkEP := channel.New(4096, uint32(mtu), "")
	if err := u.stack.CreateNIC(nicID, linkEP); err != nil {
		return nil, fmt.Errorf("create nic: %s", err)
	}
	if err := u.stack.SetPromiscuousMode(nicID, true); err != nil {
		return nil, fmt.Errorf("promiscuous: %s", err)
	}
	if err := u.stack.SetSpoofing(nicID, true); err != nil {
		return nil, fmt.Errorf("spoofing: %s", err)
	}
	u.stack.AddRoute(tcpip.Route{Destination: header.IPv4EmptySubnet, NIC: nicID})

	ctx, cancel := context.WithCancel(context.Background())
	u.wg.Add(2)
	go func() {
		defer u.wg.Done()
		// gVisor -> WG device (encrypt to client)
		for {
			pkt := linkEP.ReadContext(ctx)
			if pkt == nil {
				return
			}
			view := pkt.ToView()
			data := view.AsSlice()
			if len(data) > 0 {
				out := make([]byte, len(data))
				copy(out, data)
				select {
				case ct.outbound <- out:
				case <-ct.closed:
				}
			}
			view.Release()
			pkt.DecRef()
		}
	}()
	go func() {
		defer u.wg.Done()
		// WG device (decrypted from client) -> gVisor
		for {
			select {
			case pkt := <-ct.inbound:
				if len(pkt) < 1 || pkt[0]>>4 != 4 {
					continue
				}
				pb := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(pkt)})
				linkEP.InjectInbound(header.IPv4ProtocolNumber, pb)
				pb.DecRef()
			case <-ct.closed:
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	u.mu.Lock()
	u.pumps = append(u.pumps, closerFunc(func() error { cancel(); return nil }))
	u.mu.Unlock()
	return ct, nil
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// setup registers the TCP/UDP forwarders and the ICMP echo relay once.
func (u *userspaceBackend) setup(_ []string, _ []string) error {
	tcpFwd := tcp.NewForwarder(u.stack, 0, 4096, u.handleTCP)
	u.stack.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpFwd.HandlePacket)
	udpFwd := udp.NewForwarder(u.stack, u.handleUDP)
	u.stack.SetTransportProtocolHandler(udp.ProtocolNumber, udpFwd.HandlePacket)

	var wq waiter.Queue
	ep, err := icmp.NewProtocol4(u.stack).NewRawEndpoint(header.IPv4ProtocolNumber, &wq)
	if err != nil {
		return fmt.Errorf("icmp raw endpoint: %s", err)
	}
	u.icmpEp = ep
	u.icmpWq = &wq
	u.icmpTable = newICMPSessionTable(2 * time.Minute)
	// Register the waiter entry before starting the goroutine: a Close
	// racing ahead of the registration would lose the wakeup and strand
	// the loop on the channel.
	entry, ch := waiter.NewChannelEntry(waiter.EventIn)
	u.icmpWq.EventRegister(&entry)
	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		u.icmpReadLoop(&entry, ch)
	}()
	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		u.sessions.reaper()
	}()
	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		u.icmpTable.reaper()
	}()
	log.Printf("userspace NAT: forwarding TCP/UDP in userspace; ICMP echo relayed via host ping sockets (no direct fallback)")
	return nil
}

func (u *userspaceBackend) cleanup(_ []string, _ []string) error {
	u.once.Do(func() {
		u.mu.Lock()
		u.stopped = true
		u.cancel()
		conns := make([]net.Conn, 0, len(u.tcpConns))
		for c := range u.tcpConns {
			conns = append(conns, c)
		}
		u.mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
		for _, p := range u.pumps {
			p.Close()
		}
		u.sessions.closeAll()
		if u.icmpTable != nil {
			u.icmpTable.closeAll()
		}
		if u.icmpEp != nil {
			u.icmpEp.Close()
		}
		u.stack.Close()
		u.wg.Wait()
	})
	return nil
}

func (u *userspaceBackend) beginForward() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.stopped {
		return false
	}
	u.wg.Add(1)
	return true
}

const tcpMaxSessions = 4096

func (u *userspaceBackend) serveTCP(c net.Conn, serve func()) bool {
	u.mu.Lock()
	if u.stopped || len(u.tcpConns) >= tcpMaxSessions {
		u.mu.Unlock()
		c.Close()
		return false
	}
	u.tcpConns[c] = struct{}{}
	u.wg.Add(1)
	u.mu.Unlock()
	go func() {
		defer u.wg.Done()
		defer c.Close()
		defer func() { u.mu.Lock(); delete(u.tcpConns, c); u.mu.Unlock() }()
		serve()
	}()
	return true
}

func (u *userspaceBackend) handleTCP(req *tcp.ForwarderRequest) {
	if !u.beginForward() {
		req.Complete(true)
		return
	}
	defer u.wg.Done()
	id := req.ID()
	dst := net.JoinHostPort(id.LocalAddress.String(), fmt.Sprintf("%d", id.LocalPort))
	if h := u.interceptor(dst); h != nil {
		var wq waiter.Queue
		ep, terr := req.CreateEndpoint(&wq)
		if terr != nil {
			req.Complete(true)
			return
		}
		req.Complete(false)
		inConn := gonet.NewTCPConn(&wq, ep)
		u.serveTCP(inConn, func() { h(inConn) })
		return
	}
	outConn, err := (&net.Dialer{Timeout: 15 * time.Second}).DialContext(u.ctx, "tcp", dst)
	if err != nil {
		req.Complete(true) // RST
		return
	}
	var wq waiter.Queue
	ep, terr := req.CreateEndpoint(&wq)
	if terr != nil {
		req.Complete(true)
		outConn.Close()
		return
	}
	req.Complete(false)
	inConn := gonet.NewTCPConn(&wq, ep)
	if !u.serveTCP(outConn, func() { proxyTCP(inConn, outConn) }) {
		inConn.Close()
	}
}

func proxyTCP(a, b net.Conn) {
	type closeWriter interface{ CloseWrite() error }
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, err := io.Copy(dst, src)
		if err != nil {
			a.Close()
			b.Close()
		} else if cw, ok := dst.(closeWriter); ok {
			_ = cw.CloseWrite()
		} else {
			dst.Close()
		}
		done <- struct{}{}
	}
	go cp(b, a)
	go cp(a, b)
	<-done
	<-done
	a.Close()
	b.Close()
}

// udpFlow identifies one UDP conversation.
type udpFlow struct {
	srcIP   string
	srcPort uint16
	dstIP   string
	dstPort uint16
}

type udpSession struct {
	flow       udpFlow
	inConn     *gonet.UDPConn // connected to the WG client
	outConn    *net.UDPConn   // connected to the destination
	lastActive time.Time
	mu         sync.Mutex
	closed     bool
}

func (s *udpSession) touch() {
	s.mu.Lock()
	s.lastActive = time.Now()
	s.mu.Unlock()
}

func (s *udpSession) close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		if s.inConn != nil {
			s.inConn.Close()
		}
		if s.outConn != nil {
			s.outConn.Close()
		}
	}
	s.mu.Unlock()
}

type udpSessionTable struct {
	mu       sync.Mutex
	m        map[udpFlow]*udpSession
	timeout  time.Duration
	interval time.Duration
	closed   chan struct{}
	once     sync.Once
}

// udpMaxSessions bounds the userspace-NAT UDP session table; each
// session costs two goroutines and two sockets.
const udpMaxSessions = 4096

func newUDPSessionTable(timeout time.Duration) *udpSessionTable {
	return &udpSessionTable{m: map[udpFlow]*udpSession{}, timeout: timeout, interval: 30 * time.Second, closed: make(chan struct{})}
}

func (t *udpSessionTable) get(flow udpFlow) *udpSession {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.m[flow]
}

func (t *udpSessionTable) len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.m)
}

func (t *udpSessionTable) putIfAbsent(flow udpFlow, s *udpSession) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	select {
	case <-t.closed:
		return false
	default:
	}
	if _, exists := t.m[flow]; exists || len(t.m) >= udpMaxSessions {
		return false
	}
	t.m[flow] = s
	return true
}

func (t *udpSessionTable) remove(flow udpFlow, s *udpSession) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m[flow] == s {
		delete(t.m, flow)
	}
}

// reaper closes sessions idle longer than the timeout. The caller owns the
// goroutine lifecycle (wg.Add before go); Add inside the goroutine would
// race a concurrent Wait.
func (t *udpSessionTable) reaper() {
	tick := time.NewTicker(t.interval)
	defer tick.Stop()
	for {
		select {
		case <-t.closed:
			return
		case now := <-tick.C:
			t.mu.Lock()
			for f, s := range t.m {
				s.mu.Lock()
				idle := now.Sub(s.lastActive)
				s.mu.Unlock()
				if idle > t.timeout {
					delete(t.m, f)
					s.close()
				}
			}
			t.mu.Unlock()
		}
	}
}

func (t *udpSessionTable) closeAll() {
	t.once.Do(func() {
		close(t.closed)
		t.mu.Lock()
		for f, s := range t.m {
			delete(t.m, f)
			s.close()
		}
		t.mu.Unlock()
	})
}

// handleUDP forwards a new UDP flow.
func (u *userspaceBackend) handleUDP(req *udp.ForwarderRequest) {
	if !u.beginForward() {
		return
	}
	defer u.wg.Done()
	id := req.ID()
	flow := udpFlow{
		srcIP:   id.RemoteAddress.String(),
		srcPort: id.RemotePort,
		dstIP:   id.LocalAddress.String(),
		dstPort: id.LocalPort,
	}
	if u.sessions.get(flow) != nil {
		return // existing session owns subsequent packets
	}
	if h := u.udpInterceptor(net.JoinHostPort(flow.dstIP, fmt.Sprintf("%d", flow.dstPort))); h != nil {
		var wq waiter.Queue
		ep, terr := req.CreateEndpoint(&wq)
		if terr != nil {
			return
		}
		go h(gonet.NewUDPConn(&wq, ep))
		return
	}
	// each session costs 2 goroutines + 2 sockets + buffers; a peer
	// sweeping dst ports would otherwise exhaust memory/fd in minutes.
	if u.sessions.len() >= udpMaxSessions {
		log.Printf("userspace-nat: udp session table full (%d), dropping new flow %v", udpMaxSessions, flow)
		return
	}
	dst := &net.UDPAddr{IP: net.ParseIP(flow.dstIP), Port: int(flow.dstPort)}
	outConn, err := net.DialUDP("udp", nil, dst)
	if err != nil {
		return
	}
	var wq waiter.Queue
	ep, terr := req.CreateEndpoint(&wq)
	if terr != nil {
		outConn.Close()
		return
	}
	sess := &udpSession{
		flow:       flow,
		inConn:     gonet.NewUDPConn(&wq, ep),
		outConn:    outConn,
		lastActive: time.Now(),
	}
	if !u.sessions.putIfAbsent(flow, sess) {
		sess.close()
		return
	}

	u.wg.Add(2)
	go func() { // client -> destination
		defer u.wg.Done()
		defer u.sessions.remove(flow, sess)
		buf := make([]byte, 64*1024)
		for {
			n, err := sess.inConn.Read(buf)
			if err != nil {
				sess.close()
				return
			}
			sess.touch()
			if _, err := sess.outConn.Write(buf[:n]); err != nil {
				sess.close()
				return
			}
		}
	}()
	go func() { // destination -> client
		defer u.wg.Done()
		buf := make([]byte, 64*1024)
		for {
			n, err := sess.outConn.Read(buf)
			if err != nil {
				sess.close()
				return
			}
			sess.touch()
			if _, err := sess.inConn.Write(buf[:n]); err != nil {
				sess.close()
				return
			}
		}
	}()
}

// icmpFlow identifies one ICMP echo conversation; the ident keeps
// concurrent pings from one VM to one destination separate.
type icmpFlow struct {
	srcIP string
	dstIP string
	ident uint16
}

// icmpSession relays one echo conversation. route is held for the whole
// session (not per packet) because it pins the stack's temporary address
// for the spoofed destination; conn is the host-side ping socket.
type icmpSession struct {
	flow       icmpFlow
	route      *stack.Route
	conn       net.Conn
	lastActive time.Time
	mu         sync.Mutex
	closed     bool
}

func (s *icmpSession) touch() {
	s.mu.Lock()
	s.lastActive = time.Now()
	s.mu.Unlock()
}

func (s *icmpSession) close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		if s.conn != nil {
			s.conn.Close()
		}
		if s.route != nil {
			s.route.Release()
		}
	}
	s.mu.Unlock()
}

type icmpSessionTable struct {
	mu       sync.Mutex
	m        map[icmpFlow]*icmpSession
	timeout  time.Duration
	interval time.Duration
	closed   chan struct{}
	once     sync.Once
}

// icmpMaxSessions bounds the userspace-NAT ICMP session table; each
// session costs a goroutine, a socket and a pinned route.
const icmpMaxSessions = 1024

func newICMPSessionTable(timeout time.Duration) *icmpSessionTable {
	return &icmpSessionTable{m: map[icmpFlow]*icmpSession{}, timeout: timeout, interval: 30 * time.Second, closed: make(chan struct{})}
}

func (t *icmpSessionTable) get(flow icmpFlow) *icmpSession {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.m[flow]
}

func (t *icmpSessionTable) len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.m)
}

func (t *icmpSessionTable) putIfAbsent(flow icmpFlow, s *icmpSession) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	select {
	case <-t.closed:
		return false
	default:
	}
	if _, exists := t.m[flow]; exists || len(t.m) >= icmpMaxSessions {
		return false
	}
	t.m[flow] = s
	return true
}

func (t *icmpSessionTable) remove(flow icmpFlow, s *icmpSession) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m[flow] == s {
		delete(t.m, flow)
	}
}

// reaper closes sessions idle longer than the timeout. The caller owns the
// goroutine lifecycle (wg.Add before go); Add inside the goroutine would
// race a concurrent Wait.
func (t *icmpSessionTable) reaper() {
	tick := time.NewTicker(t.interval)
	defer tick.Stop()
	for {
		select {
		case <-t.closed:
			return
		case now := <-tick.C:
			t.mu.Lock()
			for f, s := range t.m {
				s.mu.Lock()
				idle := now.Sub(s.lastActive)
				s.mu.Unlock()
				if idle > t.timeout {
					delete(t.m, f)
					s.close()
				}
			}
			t.mu.Unlock()
		}
	}
}

func (t *icmpSessionTable) closeAll() {
	t.once.Do(func() {
		close(t.closed)
		t.mu.Lock()
		for f, s := range t.m {
			delete(t.m, f)
			s.close()
		}
		t.mu.Unlock()
	})
}

// icmpReadLoop drains the raw ICMPv4 endpoint; the stack delivers a copy
// of every inbound ICMPv4 packet here (the NICs are spoofing, so the
// stack itself never answers echo).
func (u *userspaceBackend) icmpReadLoop(entry *waiter.Entry, ch chan struct{}) {
	defer u.icmpWq.EventUnregister(entry)
	for {
		<-ch // endpoint Close wakes this; Read then fails with ErrClosedForReceive
		for {
			var buf bytes.Buffer
			res, err := u.icmpEp.Read(&buf, tcpip.ReadOptions{NeedRemoteAddr: true})
			if err != nil {
				if _, ok := err.(*tcpip.ErrWouldBlock); ok {
					break
				}
				return
			}
			u.handleICMPPacket(buf.Bytes(), res.RemoteAddr)
		}
	}
}

// handleICMPPacket relays one ICMPv4 packet from a VM. Only echo requests
// are forwarded; every other ICMP type is dropped explicitly (fail
// closed, never fall back to direct).
func (u *userspaceBackend) handleICMPPacket(data []byte, remote tcpip.FullAddress) {
	if len(data) < header.IPv4MinimumSize || data[0]>>4 != 4 {
		return
	}
	iph := header.IPv4(data)
	msg := iph.Payload()
	if len(msg) < header.ICMPv4MinimumSize {
		return
	}
	if header.ICMPv4(msg).Type() != header.ICMPv4Echo {
		log.Printf("userspace-nat: dropping unsupported icmp type %d from %s", header.ICMPv4(msg).Type(), iph.SourceAddress())
		return
	}
	src, dst := iph.SourceAddress(), iph.DestinationAddress()
	if u.icmpIsLocal(dst.String()) {
		u.answerICMPLocally(iph, msg, remote.NIC)
		return
	}
	flow := icmpFlow{srcIP: src.String(), dstIP: dst.String(), ident: header.ICMPv4(msg).Ident()}
	if sess := u.icmpTable.get(flow); sess != nil {
		sess.touch()
		if _, err := sess.conn.Write(msg); err != nil {
			sess.close()
			u.icmpTable.remove(flow, sess)
		}
		return
	}
	// each session costs a goroutine + a socket + a pinned route; a peer
	// sweeping idents would otherwise exhaust memory/fd in minutes.
	if u.icmpTable.len() >= icmpMaxSessions {
		log.Printf("userspace-nat: icmp session table full (%d), dropping flow %v", icmpMaxSessions, flow)
		return
	}
	sess, err := u.newICMPSession(flow, src, dst, remote.NIC)
	if err != nil {
		log.Printf("userspace-nat: icmp session %v: %v (dropping, no fallback)", flow, err)
		return
	}
	if !u.icmpTable.putIfAbsent(flow, sess) {
		sess.close()
		return
	}
	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		u.icmpReplyLoop(sess)
	}()
	if _, err := sess.conn.Write(msg); err != nil {
		sess.close()
		u.icmpTable.remove(flow, sess)
	}
}

// answerICMPLocally turns an echo request to one of our own tunnel
// addresses into an echo reply sourced from that address; ident, sequence
// and payload pass through unchanged.
func (u *userspaceBackend) answerICMPLocally(iph header.IPv4, msg []byte, nicID tcpip.NICID) {
	src, dst := iph.SourceAddress(), iph.DestinationAddress()
	reply := make([]byte, len(msg))
	copy(reply, msg)
	rh := header.ICMPv4(reply)
	rh.SetType(header.ICMPv4EchoReply)
	rh.SetCode(0)
	rh.SetChecksum(0)
	rh.SetChecksum(header.ICMPv4Checksum(reply[:header.ICMPv4MinimumSize], checksum.Checksum(reply[header.ICMPv4MinimumSize:], 0)))
	route, err := u.stack.FindRoute(nicID, dst, src, header.IPv4ProtocolNumber, false)
	if err != nil {
		log.Printf("userspace-nat: local icmp reply route: %s (dropping packet)", err)
		return
	}
	defer route.Release()
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		ReserveHeaderBytes: header.IPv4MaximumHeaderSize,
		Payload:            buffer.MakeWithData(reply),
	})
	werr := route.WritePacket(stack.NetworkHeaderParams{
		Protocol: header.ICMPv4ProtocolNumber,
		TTL:      64,
	}, pkt)
	pkt.DecRef()
	if werr != nil {
		log.Printf("userspace-nat: local icmp reply to %s: %s (dropping packet)", src, werr)
	}
}

// newICMPSession dials the host-side ping socket and pins the reply route.
func (u *userspaceBackend) newICMPSession(flow icmpFlow, src, dst tcpip.Address, nicID tcpip.NICID) (*icmpSession, error) {
	// Host egress is a ping socket (SOCK_DGRAM ICMP): no root needed on
	// Linux when net.ipv4.ping_group_range covers us. Without it the flow
	// fails here and is dropped, never falling back to UDP/TCP/direct.
	dial := u.icmpDial
	if dial == nil {
		dial = func(network, address string) (net.Conn, error) {
			return net.DialTimeout(network, address, 5*time.Second)
		}
	}
	conn, err := dial("ip4:icmp", flow.dstIP)
	if err != nil {
		return nil, fmt.Errorf("ping socket: %w", err)
	}
	// Replies must be sourced from the original destination. The NICs are
	// address-less (spoofing), so a route lookup only succeeds via the
	// temporary address the stack created for dst while delivering the
	// request; holding the route pins that address for the session.
	route, rerr := u.stack.FindRoute(nicID, dst, src, header.IPv4ProtocolNumber, false)
	if rerr != nil {
		conn.Close()
		return nil, fmt.Errorf("reply route: %s", rerr)
	}
	return &icmpSession{flow: flow, route: route, conn: conn, lastActive: time.Now()}, nil
}

// icmpReplyLoop forwards host-side echo replies back through the tunnel.
func (u *userspaceBackend) icmpReplyLoop(sess *icmpSession) {
	defer u.icmpTable.remove(sess.flow, sess)
	defer sess.close()
	buf := make([]byte, 65535+header.IPv4MaximumHeaderSize)
	for {
		n, err := sess.conn.Read(buf)
		if err != nil {
			return
		}
		sess.touch()
		msg := buf[:n]
		// Raw ICMP sockets (BSD/macOS) prepend the IP header; Linux ping
		// sockets do not.
		if len(msg) >= header.IPv4MinimumSize && msg[0]>>4 == 4 {
			msg = header.IPv4(msg).Payload()
		}
		if len(msg) < header.ICMPv4MinimumSize || header.ICMPv4(msg).Type() != header.ICMPv4EchoReply {
			continue
		}
		if !icmpRewriteReplyIdent(msg, sess.flow.ident) {
			continue
		}
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			ReserveHeaderBytes: header.IPv4MaximumHeaderSize,
			Payload:            buffer.MakeWithData(msg),
		})
		sess.mu.Lock()
		if sess.closed {
			sess.mu.Unlock()
			pkt.DecRef()
			return
		}
		werr := sess.route.WritePacket(stack.NetworkHeaderParams{
			Protocol: header.ICMPv4ProtocolNumber,
			TTL:      64,
		}, pkt)
		sess.mu.Unlock()
		pkt.DecRef()
		if werr != nil {
			log.Printf("userspace-nat: icmp reply to %s: %s (dropping packet)", sess.flow.srcIP, werr)
		}
	}
}

// icmpRewriteReplyIdent restores the VM's original echo ident (the kernel
// ping socket rewrites ident to its socket port) and recomputes the ICMP
// checksum. It reports whether msg is a well-formed ICMPv4 message.
func icmpRewriteReplyIdent(msg []byte, ident uint16) bool {
	if len(msg) < header.ICMPv4MinimumSize {
		return false
	}
	h := header.ICMPv4(msg)
	h.SetIdent(ident)
	h.SetChecksum(0)
	h.SetChecksum(header.ICMPv4Checksum(msg[:header.ICMPv4MinimumSize], checksum.Checksum(msg[header.ICMPv4MinimumSize:], 0)))
	return true
}

package exit

import (
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
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
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
	wg       sync.WaitGroup
	once     sync.Once

	intercept   map[string]func(net.Conn) // dst addr -> in-stack handler
	interceptMu sync.RWMutex
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

func newUserspaceBackend() *userspaceBackend {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocolCUBIC, udp.NewProtocol},
	})
	return &userspaceBackend{
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

// setup registers the TCP/UDP forwarders once.
func (u *userspaceBackend) setup(_ []string, _ []string) error {
	tcpFwd := tcp.NewForwarder(u.stack, 0, 4096, u.handleTCP)
	u.stack.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpFwd.HandlePacket)
	udpFwd := udp.NewForwarder(u.stack, u.handleUDP)
	u.stack.SetTransportProtocolHandler(udp.ProtocolNumber, udpFwd.HandlePacket)
	go u.sessions.reaper(&u.wg)
	log.Printf("userspace NAT: forwarding TCP/UDP in userspace; ICMP is NOT forwarded (ping through the tunnel will fail, expected)")
	return nil
}

func (u *userspaceBackend) cleanup(_ []string, _ []string) error {
	u.once.Do(func() {
		for _, p := range u.pumps {
			p.Close()
		}
		u.sessions.closeAll()
		u.stack.Close()
		u.wg.Wait()
	})
	return nil
}

// handleTCP dials the original destination from the VPS and relays,
// unless the destination is an intercepted in-stack service.
func (u *userspaceBackend) handleTCP(req *tcp.ForwarderRequest) {
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
		go h(gonet.NewTCPConn(&wq, ep))
		return
	}
	outConn, err := (&net.Dialer{Timeout: 15 * time.Second}).Dial("tcp", dst)
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
	go proxyTCP(inConn, outConn)
}

// proxyTCP relays full-duplex, propagating half-closes.
func proxyTCP(a, b net.Conn) {
	type closeWriter interface{ CloseWrite() error }
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(closeWriter); ok {
			_ = cw.CloseWrite()
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

func (t *udpSessionTable) put(flow udpFlow, s *udpSession) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.m[flow] = s
}

func (t *udpSessionTable) remove(flow udpFlow, s *udpSession) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m[flow] == s {
		delete(t.m, flow)
	}
}

// reaper closes sessions idle longer than the timeout.
func (t *udpSessionTable) reaper(wg *sync.WaitGroup) {
	wg.Add(1)
	defer wg.Done()
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
	u.sessions.put(flow, sess)

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

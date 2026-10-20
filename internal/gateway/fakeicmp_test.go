package gateway

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/huangzheng2016/pvm/internal/gw/dns"
	gwpolicy "github.com/huangzheng2016/pvm/internal/gw/policy"
	"github.com/huangzheng2016/pvm/internal/gw/rules"
	"golang.org/x/sys/unix"
)

// fakeICMPListener is an in-memory icmpPacketConn: injected packets are
// delivered to ReadMsgIP with their original-destination oob, and
// WriteToIP captures what the relay sends back downstream.
type fakeICMPListener struct {
	in     chan fakeICMPPacket
	closed chan struct{}
	once   sync.Once

	mu   sync.Mutex
	out  []fakeICMPPacket
	addr net.Addr
}

type fakeICMPPacket struct {
	payload []byte
	oob     []byte
	addr    net.Addr
}

func newFakeICMPListener() *fakeICMPListener {
	return &fakeICMPListener{in: make(chan fakeICMPPacket, 16), closed: make(chan struct{})}
}

func (f *fakeICMPListener) ReadMsgIP(b, oob []byte) (int, int, int, *net.IPAddr, error) {
	select {
	case pkt := <-f.in:
		n := copy(b, pkt.payload)
		oobn := copy(oob, pkt.oob)
		addr, _ := pkt.addr.(*net.IPAddr)
		return n, oobn, 0, addr, nil
	case <-f.closed:
		return 0, 0, 0, nil, net.ErrClosed
	}
}

func (f *fakeICMPListener) WriteToIP(b []byte, addr *net.IPAddr) (int, error) {
	f.mu.Lock()
	f.out = append(f.out, fakeICMPPacket{payload: append([]byte(nil), b...), addr: addr})
	f.mu.Unlock()
	return len(b), nil
}

func (f *fakeICMPListener) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

func (f *fakeICMPListener) inject(payload, oob []byte, src net.Addr) {
	f.in <- fakeICMPPacket{payload: payload, oob: oob, addr: src}
}

func (f *fakeICMPListener) written() []fakeICMPPacket {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeICMPPacket(nil), f.out...)
}

// origDstOOB builds the IP_RECVORIGDSTADDR control message carrying ip,
// in the sockaddr_in layout originalDstUDP parses.
func origDstOOB(t *testing.T, ip net.IP) []byte {
	t.Helper()
	v4 := ip.To4()
	if v4 == nil {
		t.Fatalf("origDstOOB: %v is not IPv4", ip)
	}
	data := make([]byte, 16)
	copy(data[4:8], v4)
	b := make([]byte, unix.CmsgSpace(16))
	hdr := (*unix.Cmsghdr)(unsafe.Pointer(&b[0]))
	hdr.SetLen(unix.CmsgLen(16))
	hdr.Level = 0 // SOL_IP
	hdr.Type = ipOriginalDst
	copy(b[unix.CmsgLen(0):], data)
	return b
}

func buildEcho(typ byte, id, seq int, payload []byte) []byte {
	msg := make([]byte, 8+len(payload))
	msg[0] = typ
	binary.BigEndian.PutUint16(msg[4:6], uint16(id))
	binary.BigEndian.PutUint16(msg[6:8], uint16(seq))
	copy(msg[8:], payload)
	icmpSetChecksum(msg)
	return msg
}

func icmpChecksumValid(msg []byte) bool {
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
	return uint16(sum) == 0xffff
}

func TestFakeICMPRelayRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name  string
		proxy bool
	}{
		{"Direct", false},
		{"Proxy", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng, pool := newFakeUDPTestPolicy(t)
			if tc.proxy {
				eng.SetDefaultExit("my-exit")
			}
			fake := pool.Allocate("example.com")
			p := newFakeICMP(eng, "10.0.2.3", func(net.IP) string { return "dev1" })
			wantAddr, wantMark := "127.0.0.1", -1
			if tc.proxy {
				wantAddr, wantMark = "203.0.113.7", 5
				p.SetExitHooks(func(string) int { return 5 },
					func(string) (string, bool) { return "", false },
					func(string) (string, bool) { return "", false })
				p.lookupProxy = func(_ context.Context, domain, exitName string, mark int) (net.IP, error) {
					if domain != "example.com" || exitName != "my-exit" || mark != 5 {
						t.Errorf("lookupProxy(%q, %q, %d)", domain, exitName, mark)
					}
					return net.ParseIP("203.0.113.7"), nil
				}
			} else {
				p.lookupDirect = func(_ context.Context, domain string) (net.IP, error) {
					if domain != "example.com" {
						t.Errorf("lookupDirect(%q)", domain)
					}
					return net.ParseIP("127.0.0.1"), nil
				}
			}
			ln := newFakeICMPListener()
			p.ln = ln
			p.wg.Add(1)
			go p.readLoop(ln)
			t.Cleanup(p.Close)

			targetCh := make(chan net.Conn, 1)
			p.dialICMP = func(_ context.Context, address string, mark int) (net.Conn, error) {
				if address != wantAddr || mark != wantMark {
					t.Errorf("dialICMP(%q, %d), want (%q, %d)", address, mark, wantAddr, wantMark)
				}
				relay, target := net.Pipe()
				targetCh <- target
				return relay, nil
			}

			gotReq := make(chan []byte, 1)
			go func() {
				conn := <-targetCh
				defer conn.Close()
				buf := make([]byte, 2048)
				n, err := conn.Read(buf)
				if err != nil {
					return
				}
				gotReq <- append([]byte(nil), buf[:n]...)
				reply := buildEcho(icmpEchoReply,
					int(binary.BigEndian.Uint16(buf[4:6])), int(binary.BigEndian.Uint16(buf[6:8])), buf[8:n])
				conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
				conn.Write(reply)
			}()

			src := &net.IPAddr{IP: net.ParseIP("192.168.1.10")}
			ln.inject(buildEcho(icmpEchoRequest, 0x1234, 7, []byte("ping")), origDstOOB(t, fake), src)

			var req []byte
			select {
			case req = <-gotReq:
			case <-time.After(3 * time.Second):
				t.Fatal("echo request was not relayed upstream")
			}
			if req[0] != icmpEchoRequest {
				t.Fatalf("upstream type = %d, want echo request", req[0])
			}
			if id := int(binary.BigEndian.Uint16(req[4:6])); id == 0x1234 {
				t.Fatal("upstream identifier was not rewritten")
			}
			if !icmpChecksumValid(req) {
				t.Fatal("upstream checksum invalid")
			}

			deadline := time.Now().Add(3 * time.Second)
			var out []fakeICMPPacket
			for time.Now().Before(deadline) {
				if out = ln.written(); len(out) > 0 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if len(out) == 0 {
				t.Fatal("echo reply was not relayed downstream")
			}
			pkt := out[0]
			if pkt.addr.String() != src.String() {
				t.Errorf("reply destination = %v, want %v", pkt.addr, src)
			}
			reply := pkt.payload
			if reply[0] != icmpEchoReply {
				t.Errorf("downstream type = %d, want echo reply", reply[0])
			}
			if id := int(binary.BigEndian.Uint16(reply[4:6])); id != 0x1234 {
				t.Errorf("downstream id = %#x, want 0x1234", id)
			}
			if seq := int(binary.BigEndian.Uint16(reply[6:8])); seq != 7 {
				t.Errorf("downstream seq = %d, want 7", seq)
			}
			if string(reply[8:]) != "ping" {
				t.Errorf("downstream payload = %q, want ping", reply[8:])
			}
			if !icmpChecksumValid(reply) {
				t.Fatal("downstream checksum invalid")
			}
		})
	}
}

// The proxied resolve must go through the exit's domain-connect service
// (WriteDomainResolve handshake) when one is configured.
func TestFakeICMPProxyDomainConnectResolve(t *testing.T) {
	eng, pool := newFakeUDPTestPolicy(t)
	eng.SetDefaultExit("my-exit")
	fake := pool.Allocate("example.com")

	ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.AcceptTCP()
		if err != nil {
			return
		}
		defer conn.Close()
		hdr := make([]byte, 4)
		if _, err := io.ReadFull(conn, hdr); err != nil {
			return
		}
		if hdr[0] != 1 || hdr[1] != 2 {
			t.Errorf("domain-connect frame = %v, want ver 1 cmd 2", hdr)
			return
		}
		buf := make([]byte, int(binary.BigEndian.Uint16(hdr[2:4])))
		if _, err := io.ReadFull(conn, buf); err != nil {
			return
		}
		if string(buf) != "example.com" {
			t.Errorf("domain-connect domain = %q, want example.com", buf)
			return
		}
		msg := []byte("203.0.113.7")
		conn.Write(append([]byte{0, byte(len(msg) >> 8), byte(len(msg))}, msg...))
	}()

	p := newFakeICMP(eng, "10.0.2.3", nil)
	p.SetExitHooks(func(string) int { return 5 },
		func(string) (string, bool) { return ln.Addr().String(), true },
		func(string) (string, bool) { return "", false })
	p.dialDC = func(ctx context.Context, address string, mark int) (net.Conn, error) {
		if mark != 5 {
			t.Errorf("domain-connect dial mark = %d, want 5", mark)
		}
		var d net.Dialer
		return d.DialContext(ctx, "tcp4", address)
	}
	relay, target := net.Pipe()
	defer target.Close()
	p.dialICMP = func(_ context.Context, address string, mark int) (net.Conn, error) {
		if address != "203.0.113.7" || mark != 5 {
			t.Errorf("icmp dial (%q, %d), want (203.0.113.7, 5)", address, mark)
		}
		return relay, nil
	}

	key := fakeICMPKey{srcIP: "192.168.1.10", dstIP: fake.String(), id: 0x1234}
	if _, start := p.reserve(key); !start {
		t.Fatal("flow was not reserved")
	}
	go p.openFlow(key, &net.IPAddr{IP: net.ParseIP("192.168.1.10")}, fake, "example.com", buildEcho(icmpEchoRequest, 0x1234, 7, nil))

	_ = target.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 128)
	n, err := target.Read(buf)
	if err != nil {
		t.Fatalf("request not relayed after domain-connect resolve: %v", err)
	}
	if !icmpChecksumValid(buf[:n]) {
		t.Fatal("upstream checksum invalid")
	}
	p.mu.Lock()
	adopted := len(p.flows)
	p.mu.Unlock()
	if adopted != 1 {
		t.Fatalf("flows = %d, want 1", adopted)
	}
	p.Close()
}

// A proxied flow with a down tunnel must fail closed: no resolve, no
// dial, no fallback, and nothing retained.
func TestFakeICMPProxyFailsClosed(t *testing.T) {
	eng, pool := newFakeUDPTestPolicy(t)
	eng.SetDefaultExit("my-exit")
	fake := pool.Allocate("example.com")
	p := newFakeICMP(eng, "10.0.2.3", nil)
	called := false
	p.lookupProxy = func(context.Context, string, string, int) (net.IP, error) {
		called = true
		return nil, nil
	}
	p.dialICMP = func(context.Context, string, int) (net.Conn, error) {
		t.Fatal("must not dial when the tunnel is down")
		return nil, nil
	}
	key := fakeICMPKey{srcIP: "192.168.1.10", dstIP: fake.String(), id: 0x1234}
	if _, start := p.reserve(key); !start {
		t.Fatal("flow was not reserved")
	}
	p.openFlow(key, &net.IPAddr{IP: net.ParseIP("192.168.1.10")}, fake, "example.com", buildEcho(icmpEchoRequest, 0x1234, 7, nil))
	if called {
		t.Fatal("resolver must not run when the tunnel is down")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.flows) != 0 || len(p.pending) != 0 {
		t.Fatalf("failed flow was retained: flows=%d pending=%d", len(p.flows), len(p.pending))
	}
}

// A proxied resolve failure drops the packet; it must never retry via the
// host resolver (identity leak) or dial anything.
func TestFakeICMPProxyResolveFailureDrops(t *testing.T) {
	eng, pool := newFakeUDPTestPolicy(t)
	eng.SetDefaultExit("my-exit")
	fake := pool.Allocate("example.com")
	p := newFakeICMP(eng, "10.0.2.3", nil)
	p.SetExitHooks(func(string) int { return 5 },
		func(string) (string, bool) { return "", false },
		func(string) (string, bool) { return "", false })
	p.lookupDirect = func(context.Context, string) (net.IP, error) {
		t.Fatal("proxy resolve failure must not fall back to direct")
		return nil, nil
	}
	p.lookupProxy = func(context.Context, string, string, int) (net.IP, error) {
		return nil, fmt.Errorf("resolve failed")
	}
	p.dialICMP = func(context.Context, string, int) (net.Conn, error) {
		t.Fatal("must not dial after a resolve failure")
		return nil, nil
	}
	key := fakeICMPKey{srcIP: "192.168.1.10", dstIP: fake.String(), id: 0x1234}
	if _, start := p.reserve(key); !start {
		t.Fatal("flow was not reserved")
	}
	p.openFlow(key, &net.IPAddr{IP: net.ParseIP("192.168.1.10")}, fake, "example.com", buildEcho(icmpEchoRequest, 0x1234, 7, nil))
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.flows) != 0 || len(p.pending) != 0 {
		t.Fatalf("failed flow was retained: flows=%d pending=%d", len(p.flows), len(p.pending))
	}
}

func TestFakeICMPSetPolicyClosesFlippedFlow(t *testing.T) {
	eng, pool := newFakeUDPTestPolicy(t)
	fake := pool.Allocate("example.com")
	p := newFakeICMP(eng, "10.0.2.3", nil)
	relay, target := net.Pipe()
	defer target.Close()
	key := fakeICMPKey{srcIP: "192.168.1.10", dstIP: fake.String(), id: 0x1234}
	p.flows[key] = &fakeICMPFlow{key: key, domain: "example.com", vm: "dev1", route: gwpolicy.Direct, origID: 0x1234, upID: 1, up: relay}
	eng.SetVMExit("dev1", "my-exit")
	p.SetPolicy(eng)
	p.mu.Lock()
	if len(p.flows) != 0 {
		p.mu.Unlock()
		t.Fatal("flipped flow was retained")
	}
	p.mu.Unlock()
	_ = target.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := target.Read(make([]byte, 1)); err == nil {
		t.Fatal("flipped flow socket was not closed")
	}
	p.Close()
}

func TestFakeICMPSetExitHooksClosesFlowOnTunnelDown(t *testing.T) {
	eng, pool := newFakeUDPTestPolicy(t)
	fake := pool.Allocate("example.com")
	p := newFakeICMP(eng, "10.0.2.3", nil)
	relay, target := net.Pipe()
	defer target.Close()
	key := fakeICMPKey{srcIP: "192.168.1.10", dstIP: fake.String(), id: 0x1234}
	p.flows[key] = &fakeICMPFlow{key: key, domain: "example.com", vm: "dev1", route: gwpolicy.Proxy, exit: "my-exit", origID: 0x1234, upID: 1, up: relay}
	p.SetExitHooks(func(string) int { return -1 },
		func(string) (string, bool) { return "", false },
		func(string) (string, bool) { return "", false })
	p.mu.Lock()
	if len(p.flows) != 0 {
		p.mu.Unlock()
		t.Fatal("proxied flow was retained with the tunnel down")
	}
	p.mu.Unlock()
	_ = target.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := target.Read(make([]byte, 1)); err == nil {
		t.Fatal("flow socket was not closed")
	}
	p.Close()
}

// A duplicate first packet arriving while the session is still being
// opened must not create a second upstream session.
func TestFakeICMPDuplicateFirstPacketSingleSession(t *testing.T) {
	eng, pool := newFakeUDPTestPolicy(t)
	fake := pool.Allocate("example.com")
	p := newFakeICMP(eng, "10.0.2.3", nil)
	release := make(chan struct{})
	p.lookupDirect = func(context.Context, string) (net.IP, error) {
		<-release
		return net.ParseIP("127.0.0.1"), nil
	}
	var dials atomic.Int32
	targetCh := make(chan net.Conn, 1)
	p.dialICMP = func(context.Context, string, int) (net.Conn, error) {
		dials.Add(1)
		relay, target := net.Pipe()
		targetCh <- target
		return relay, nil
	}
	ln := newFakeICMPListener()
	p.ln = ln
	p.wg.Add(1)
	go p.readLoop(ln)
	t.Cleanup(p.Close)

	gotReq := make(chan []byte, 4)
	go func() {
		conn := <-targetCh
		defer conn.Close()
		buf := make([]byte, 2048)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			gotReq <- append([]byte(nil), buf[:n]...)
		}
	}()

	src := &net.IPAddr{IP: net.ParseIP("192.168.1.10")}
	pkt := buildEcho(icmpEchoRequest, 0x1234, 7, []byte("ping"))
	oob := origDstOOB(t, fake)
	ln.inject(pkt, oob, src)
	ln.inject(pkt, oob, src) // arrives while the first open is still pending
	close(release)

	select {
	case <-gotReq:
	case <-time.After(3 * time.Second):
		t.Fatal("first packet was not relayed")
	}
	time.Sleep(100 * time.Millisecond)
	if got := dials.Load(); got != 1 {
		t.Fatalf("dial calls = %d, want 1", got)
	}
	select {
	case extra := <-gotReq:
		t.Fatalf("duplicate first packet produced extra upstream traffic (%d bytes)", len(extra))
	default:
	}
}

func TestFakeICMPReapIdleClosesFlow(t *testing.T) {
	eng, pool := newFakeUDPTestPolicy(t)
	fake := pool.Allocate("example.com")
	p := newFakeICMP(eng, "10.0.2.3", nil)
	idleRelay, idleTarget := net.Pipe()
	defer idleTarget.Close()
	freshRelay, freshTarget := net.Pipe()
	defer freshTarget.Close()
	idleKey := fakeICMPKey{srcIP: "192.168.1.10", dstIP: fake.String(), id: 7}
	freshKey := fakeICMPKey{srcIP: "192.168.1.10", dstIP: fake.String(), id: 8}
	p.flows[idleKey] = &fakeICMPFlow{key: idleKey, domain: "example.com", vm: "dev1", route: gwpolicy.Direct, origID: 7, upID: 1, up: idleRelay, lastActive: time.Now().Add(-fakeICMPIdleTimeout - time.Minute)}
	p.flows[freshKey] = &fakeICMPFlow{key: freshKey, domain: "example.com", vm: "dev1", route: gwpolicy.Direct, origID: 8, upID: 2, up: freshRelay, lastActive: time.Now()}

	p.reapIdle()

	p.mu.Lock()
	_, idlePresent := p.flows[idleKey]
	_, freshPresent := p.flows[freshKey]
	p.mu.Unlock()
	if idlePresent {
		t.Fatal("idle flow was retained")
	}
	if !freshPresent {
		t.Fatal("fresh flow was reaped")
	}
	_ = idleTarget.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := idleTarget.Read(make([]byte, 1)); err == nil {
		t.Fatal("idle flow socket was not closed")
	}
	p.Close()
}

// Non-echo ICMP types (here: destination unreachable) are dropped without
// ever opening a session.
func TestFakeICMPNonEchoDropped(t *testing.T) {
	eng, pool := newFakeUDPTestPolicy(t)
	fake := pool.Allocate("example.com")
	p := newFakeICMP(eng, "10.0.2.3", nil)
	var dials atomic.Int32
	p.lookupDirect = func(context.Context, string) (net.IP, error) {
		return net.ParseIP("127.0.0.1"), nil
	}
	p.dialICMP = func(context.Context, string, int) (net.Conn, error) {
		dials.Add(1)
		relay, target := net.Pipe()
		target.Close()
		return relay, nil
	}
	ln := newFakeICMPListener()
	p.ln = ln
	p.wg.Add(1)
	go p.readLoop(ln)
	t.Cleanup(p.Close)

	src := &net.IPAddr{IP: net.ParseIP("192.168.1.10")}
	ln.inject(buildEcho(3, 0x1234, 7, []byte("x")), origDstOOB(t, fake), src)
	// a valid request afterwards proves the loop already processed the drop
	ln.inject(buildEcho(icmpEchoRequest, 0x1234, 7, nil), origDstOOB(t, fake), src)

	deadline := time.Now().Add(3 * time.Second)
	for dials.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("dial calls = %d, want 1 (non-echo must be dropped)", got)
	}
	if n := len(ln.written()); n != 0 {
		t.Fatalf("downstream writes = %d, want 0", n)
	}
}

func TestFakeICMPCloseUnblocksReadLoop(t *testing.T) {
	eng, _ := newFakeUDPTestPolicy(t)
	p := newFakeICMP(eng, "10.0.2.3", nil)
	ln := newFakeICMPListener()
	p.ln = ln
	p.wg.Add(1)
	go p.readLoop(ln)
	done := make(chan struct{})
	go func() { p.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not unblock the read loop")
	}
}

func TestIsUpstreamID(t *testing.T) {
	p := newFakeICMP(nil, "10.0.2.3", nil)
	p.flows[fakeICMPKey{id: 42}] = &fakeICMPFlow{upID: 42}
	if !p.isUpstreamID(42) {
		t.Fatal("registered upstream ID was not rejected")
	}
	if p.isUpstreamID(43) {
		t.Fatal("unrelated ID was rejected")
	}
}

func TestIsEchoReplyMatchesUpstreamID(t *testing.T) {
	if !isEchoReply(buildEcho(icmpEchoReply, 99, 1, nil), 99) {
		t.Fatal("reply with the upstream ID was rejected")
	}
	if isEchoReply(buildEcho(icmpEchoReply, 1, 1, nil), 99) {
		t.Fatal("reply with a different ID was accepted")
	}
	if isEchoReply(buildEcho(icmpEchoRequest, 99, 1, nil), 99) {
		t.Fatal("echo request was accepted as a reply")
	}
}

func TestICMPMessageStripsIPv4Header(t *testing.T) {
	echo := buildEcho(icmpEchoRequest, 1, 1, []byte("ping"))
	packet := append([]byte{0x45, 0, 0, 20, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 1}, echo...)
	if got := icmpMessage(packet); len(got) != len(echo) || got[0] != icmpEchoRequest {
		t.Fatalf("IPv4 header was not stripped: %v", got[:min(8, len(got))])
	}
	if got := icmpMessage(echo); len(got) != len(echo) {
		t.Fatal("raw ICMP message changed")
	}
}

func TestFakeICMPChecksum(t *testing.T) {
	msg := buildEcho(icmpEchoRequest, 0x1234, 7, []byte("ping"))
	if !icmpChecksumValid(msg) {
		t.Fatal("checksum invalid")
	}
	msg[8] ^= 0xff
	icmpSetChecksum(msg)
	if !icmpChecksumValid(msg) {
		t.Fatal("checksum invalid after recompute")
	}
	if odd := buildEcho(icmpEchoRequest, 1, 1, []byte("abc")); !icmpChecksumValid(odd) {
		t.Fatal("odd-length checksum invalid")
	}
}

// Real end-to-end echo through the relay over raw sockets: loopback
// stands in for the DNATed fake destination (the fake IP comes from
// 127.0.0.0/8, so IP_RECVORIGDSTADDR reports it without nft). Needs root;
// skips elsewhere.
func TestFakeICMPRealEchoLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("raw ICMP relay runs on the linux gateway")
	}
	eng, err := rules.NewEngine("", nil)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := dns.NewPool("127.0.0.0/8", 0)
	if err != nil {
		t.Fatal(err)
	}
	fake := pool.Allocate("example.com")
	p := newFakeICMP(gwpolicy.New(eng, pool), "127.0.0.1", nil)
	p.lookupDirect = func(context.Context, string) (net.IP, error) {
		return net.ParseIP("127.0.0.1"), nil
	}
	if err := p.Start("127.0.0.1"); err != nil {
		if strings.Contains(err.Error(), "operation not permitted") {
			t.Skipf("raw ICMP socket unavailable: %v", err)
		}
		t.Fatal(err)
	}
	t.Cleanup(p.Close)

	client, err := net.Dial("ip4:icmp", fake.String())
	if err != nil {
		if strings.Contains(err.Error(), "operation not permitted") {
			t.Skipf("raw ICMP socket unavailable: %v", err)
		}
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write(buildEcho(icmpEchoRequest, 0x4321, 9, []byte("ping"))); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2048)
	for {
		n, err := client.Read(buf)
		if err != nil {
			t.Fatalf("no echo reply: %v", err)
		}
		msg := icmpMessage(buf[:n])
		if len(msg) >= 8 && msg[0] == icmpEchoReply && int(binary.BigEndian.Uint16(msg[4:6])) == 0x4321 {
			if seq := int(binary.BigEndian.Uint16(msg[6:8])); seq != 9 {
				t.Fatalf("seq = %d, want 9", seq)
			}
			if string(msg[8:]) != "ping" {
				t.Fatalf("payload = %q, want ping", msg[8:])
			}
			break
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.mu.Lock()
		flows := len(p.flows)
		p.mu.Unlock()
		if flows == 1 {
			break
		}
		if flows > 1 || time.Now().After(deadline) {
			t.Fatalf("relay flows = %d, want 1 (reply must come through the relay)", flows)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

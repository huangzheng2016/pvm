package exit

import (
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

func TestUDPSessionTableReaper(t *testing.T) {
	tab := newUDPSessionTable(50 * time.Millisecond)
	tab.interval = 20 * time.Millisecond
	flow := udpFlow{srcIP: "10.99.0.2", srcPort: 1000, dstIP: "1.1.1.1", dstPort: 53}
	tab.putIfAbsent(flow, &udpSession{flow: flow, lastActive: time.Now()})

	var wg sync.WaitGroup
	done := make(chan struct{})
	go func() { tab.reaper(&wg); close(done) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if tab.get(flow) == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if tab.get(flow) != nil {
		t.Fatal("idle session not reaped")
	}
	tab.closeAll()
	wg.Wait()
	<-done
}

func TestUDPSessionTableCloseAll(t *testing.T) {
	tab := newUDPSessionTable(time.Hour)
	tab.putIfAbsent(udpFlow{srcIP: "a"}, &udpSession{lastActive: time.Now()})
	tab.putIfAbsent(udpFlow{srcIP: "b"}, &udpSession{lastActive: time.Now()})
	tab.closeAll()
	if tab.get(udpFlow{srcIP: "a"}) != nil || tab.get(udpFlow{srcIP: "b"}) != nil {
		t.Fatal("sessions not cleared")
	}
	tab.closeAll()
}

func TestUDPSessionTablePutIfAbsent(t *testing.T) {
	tab := newUDPSessionTable(time.Hour)
	flow := udpFlow{srcIP: "10.99.0.2", srcPort: 1000, dstIP: "1.1.1.1", dstPort: 53}
	s1 := &udpSession{flow: flow, lastActive: time.Now()}
	if !tab.putIfAbsent(flow, s1) {
		t.Fatal("first putIfAbsent rejected")
	}
	if tab.putIfAbsent(flow, &udpSession{flow: flow, lastActive: time.Now()}) {
		t.Fatal("duplicate putIfAbsent accepted")
	}
	if got := tab.get(flow); got != s1 {
		t.Fatal("duplicate replaced the original session")
	}

	full := newUDPSessionTable(time.Hour)
	for i := 0; i < udpMaxSessions; i++ {
		f := udpFlow{srcIP: "10.99.0.2", srcPort: uint16(i), dstIP: "1.1.1.1", dstPort: 53}
		if !full.putIfAbsent(f, &udpSession{flow: f, lastActive: time.Now()}) {
			t.Fatalf("putIfAbsent %d below cap rejected", i)
		}
	}
	if full.putIfAbsent(udpFlow{srcIP: "10.99.0.3", srcPort: 1, dstIP: "1.1.1.1", dstPort: 53}, &udpSession{lastActive: time.Now()}) {
		t.Fatal("putIfAbsent beyond cap accepted")
	}
}

func TestChanTUNReadWrite(t *testing.T) {
	ct := newChanTUN("test0", 1420)
	// WG device Write (decrypted packet) -> inbound
	pkt := []byte("ip packet")
	n, err := ct.Write([][]byte{append(make([]byte, 4), pkt...)}, 4)
	if err != nil || n != 1 {
		t.Fatalf("write: n=%d err=%v", n, err)
	}
	select {
	case got := <-ct.inbound:
		if string(got) != string(pkt) {
			t.Fatalf("inbound = %q", got)
		}
	default:
		t.Fatal("packet not queued inbound")
	}

	// outbound -> Read (packets to encrypt)
	ct.outbound <- pkt
	bufs := [][]byte{make([]byte, 2048)}
	sizes := []int{0}
	nr, err := ct.Read(bufs, sizes, 4)
	if err != nil || nr != 1 || sizes[0] != len(pkt) {
		t.Fatalf("read: nr=%d size=%d err=%v", nr, sizes[0], err)
	}

	ct.Close()
	if _, err := ct.Read(bufs, sizes, 4); err == nil {
		t.Fatal("read after close should fail")
	}
	ct.Close() // idempotent
}

func testV4Addr(t *testing.T, s string) tcpip.Address {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatal(err)
	}
	return tcpip.AddrFromSlice(a.AsSlice())
}

// testICMPPacket builds a checksummed IPv4+ICMPv4 packet.
func testICMPPacket(t *testing.T, src, dst string, icmpType header.ICMPv4Type, ident, seq uint16, payload []byte) []byte {
	t.Helper()
	msg := make([]byte, header.ICMPv4MinimumSize+len(payload))
	h := header.ICMPv4(msg)
	h.SetType(icmpType)
	h.SetCode(0)
	h.SetIdent(ident)
	h.SetSequence(seq)
	copy(msg[header.ICMPv4MinimumSize:], payload)
	h.SetChecksum(header.ICMPv4Checksum(msg[:header.ICMPv4MinimumSize], checksum.Checksum(payload, 0)))

	pkt := make([]byte, header.IPv4MinimumSize+len(msg))
	pkt[0] = 0x45 // version 4, IHL 5
	pkt[9] = 1    // ICMP
	ip := header.IPv4(pkt)
	ip.SetHeaderLength(header.IPv4MinimumSize)
	ip.SetTotalLength(uint16(len(pkt)))
	ip.SetTTL(64)
	ip.SetSourceAddress(testV4Addr(t, src))
	ip.SetDestinationAddress(testV4Addr(t, dst))
	ip.SetChecksum(^ip.CalculateChecksum())
	copy(pkt[header.IPv4MinimumSize:], msg)
	return pkt
}

// testICMPChecksumValid reports whether the one's-complement sum over the
// whole ICMP message (checksum field included) is 0xffff.
func testICMPChecksumValid(msg []byte) bool {
	var sum uint32
	for i := 0; i+1 < len(msg); i += 2 {
		sum += uint32(msg[i])<<8 | uint32(msg[i+1])
	}
	if len(msg)%2 == 1 {
		sum += uint32(msg[len(msg)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return uint16(sum) == 0xffff
}

func TestICMPRewriteReplyIdent(t *testing.T) {
	msg := make([]byte, header.ICMPv4MinimumSize+5)
	h := header.ICMPv4(msg)
	h.SetType(header.ICMPv4EchoReply)
	h.SetCode(0)
	h.SetIdent(0x4242) // kernel-rewritten ident
	h.SetSequence(7)
	copy(msg[header.ICMPv4MinimumSize:], "pvm-1")
	h.SetChecksum(header.ICMPv4Checksum(msg[:header.ICMPv4MinimumSize], checksum.Checksum(msg[header.ICMPv4MinimumSize:], 0)))
	if !testICMPChecksumValid(msg) {
		t.Fatal("test setup: bad checksum")
	}

	if !icmpRewriteReplyIdent(msg, 0x1234) {
		t.Fatal("well-formed message rejected")
	}
	if got := header.ICMPv4(msg).Ident(); got != 0x1234 {
		t.Fatalf("ident = %#x, want 0x1234", got)
	}
	if !testICMPChecksumValid(msg) {
		t.Fatal("checksum not updated after ident rewrite")
	}
	if icmpRewriteReplyIdent(msg[:4], 0x1234) {
		t.Fatal("truncated message accepted")
	}
}

func TestICMPSessionTable(t *testing.T) {
	tab := newICMPSessionTable(time.Hour)
	flow := icmpFlow{srcIP: "10.99.0.2", dstIP: "203.0.113.9", ident: 1}
	s1 := &icmpSession{flow: flow, lastActive: time.Now()}
	if !tab.putIfAbsent(flow, s1) {
		t.Fatal("first putIfAbsent rejected")
	}
	if tab.putIfAbsent(flow, &icmpSession{flow: flow, lastActive: time.Now()}) {
		t.Fatal("duplicate putIfAbsent accepted")
	}
	if got := tab.get(flow); got != s1 {
		t.Fatal("duplicate replaced the original session")
	}

	full := newICMPSessionTable(time.Hour)
	for i := 0; i < icmpMaxSessions; i++ {
		f := icmpFlow{srcIP: "10.99.0.2", dstIP: "203.0.113.9", ident: uint16(i)}
		if !full.putIfAbsent(f, &icmpSession{flow: f, lastActive: time.Now()}) {
			t.Fatalf("putIfAbsent %d below cap rejected", i)
		}
	}
	if full.putIfAbsent(icmpFlow{srcIP: "10.99.0.3", dstIP: "203.0.113.9", ident: 1}, &icmpSession{lastActive: time.Now()}) {
		t.Fatal("putIfAbsent beyond cap accepted")
	}

	tab.remove(flow, s1)
	if tab.get(flow) != nil {
		t.Fatal("remove did not delete the session")
	}
	tab.putIfAbsent(flow, s1)
	tab.closeAll()
	if tab.get(flow) != nil {
		t.Fatal("closeAll did not clear the table")
	}
	tab.closeAll() // idempotent
}

func TestICMPSessionTableReaper(t *testing.T) {
	tab := newICMPSessionTable(50 * time.Millisecond)
	tab.interval = 20 * time.Millisecond
	flow := icmpFlow{srcIP: "10.99.0.2", dstIP: "203.0.113.9", ident: 1}
	tab.putIfAbsent(flow, &icmpSession{flow: flow, lastActive: time.Now()})

	var wg sync.WaitGroup
	done := make(chan struct{})
	go func() { tab.reaper(&wg); close(done) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if tab.get(flow) == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if tab.get(flow) != nil {
		t.Fatal("idle session not reaped")
	}
	tab.closeAll()
	wg.Wait()
	<-done
}

// The whole relay path at packet level: echo request from a VM enters the
// stack, exits to the host ping socket, and the host's reply (with the
// kernel-rewritten ident) comes back as a correctly sourced, checksummed
// echo reply towards the VM. The host socket is a net.Pipe via icmpDial,
// so this runs without root on any platform.
func TestICMPRelayThroughStack(t *testing.T) {
	u := newUserspaceBackend()
	if err := u.setup(nil, nil); err != nil {
		t.Fatal(err)
	}
	defer u.cleanup(nil, nil)
	dev, err := u.newTun("test0", 1420)
	if err != nil {
		t.Fatal(err)
	}
	ct := dev.(*chanTUN)

	hostSide, relaySide := net.Pipe()
	defer hostSide.Close()
	u.icmpDial = func(network, address string) (net.Conn, error) {
		if network != "ip4:icmp" {
			t.Errorf("dial network = %q", network)
		}
		if address != "203.0.113.9" {
			t.Errorf("dial address = %q", address)
		}
		return relaySide, nil
	}

	// VM 10.99.0.2 pings 203.0.113.9.
	ct.inbound <- testICMPPacket(t, "10.99.0.2", "203.0.113.9", header.ICMPv4Echo, 0x1234, 7, []byte("pvm"))

	// The request must arrive at the host socket unchanged.
	_ = hostSide.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 2048)
	n, err := hostSide.Read(buf)
	if err != nil {
		t.Fatalf("host read: %v", err)
	}
	got := header.ICMPv4(buf[:n])
	if got.Type() != header.ICMPv4Echo || got.Ident() != 0x1234 || got.Sequence() != 7 || string(buf[n-3:n]) != "pvm" {
		t.Fatalf("relayed request = type %d ident %#x seq %d payload %q", got.Type(), got.Ident(), got.Sequence(), buf[n-3:n])
	}

	// Host kernel answers with the ident rewritten to its socket port.
	reply := make([]byte, header.ICMPv4MinimumSize+3)
	rh := header.ICMPv4(reply)
	rh.SetType(header.ICMPv4EchoReply)
	rh.SetCode(0)
	rh.SetIdent(0x4242)
	rh.SetSequence(7)
	copy(reply[header.ICMPv4MinimumSize:], "pvm")
	rh.SetChecksum(header.ICMPv4Checksum(reply[:header.ICMPv4MinimumSize], checksum.Checksum(reply[header.ICMPv4MinimumSize:], 0)))
	if _, err := hostSide.Write(reply); err != nil {
		t.Fatal(err)
	}

	// The reply must come out the tunnel side sourced from the original
	// destination with the VM's ident restored and a valid checksum.
	select {
	case out := <-ct.outbound:
		iph := header.IPv4(out)
		if iph.SourceAddress().String() != "203.0.113.9" || iph.DestinationAddress().String() != "10.99.0.2" {
			t.Fatalf("reply ip src=%s dst=%s", iph.SourceAddress(), iph.DestinationAddress())
		}
		msg := iph.Payload()
		mh := header.ICMPv4(msg)
		if mh.Type() != header.ICMPv4EchoReply || mh.Ident() != 0x1234 || mh.Sequence() != 7 {
			t.Fatalf("reply icmp type=%d ident=%#x seq=%d", mh.Type(), mh.Ident(), mh.Sequence())
		}
		if !testICMPChecksumValid(msg) {
			t.Fatal("reply checksum invalid")
		}
		if string(msg[header.ICMPv4MinimumSize:]) != "pvm" {
			t.Fatalf("reply payload = %q", msg[header.ICMPv4MinimumSize:])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no relayed reply")
	}

	// Second request on the same flow reuses the session; the host answer
	// may carry an IP header (BSD raw sockets) which the relay must strip.
	ct.inbound <- testICMPPacket(t, "10.99.0.2", "203.0.113.9", header.ICMPv4Echo, 0x1234, 8, []byte("two"))
	_ = hostSide.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err = hostSide.Read(buf)
	if err != nil {
		t.Fatalf("host read 2: %v", err)
	}
	if got := header.ICMPv4(buf[:n]); got.Sequence() != 8 {
		t.Fatalf("second request seq = %d", got.Sequence())
	}
	reply2 := testICMPPacket(t, "203.0.113.9", "10.99.0.2", header.ICMPv4EchoReply, 0x4242, 8, []byte("two"))
	if _, err := hostSide.Write(reply2); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-ct.outbound:
		iph := header.IPv4(out)
		if iph.SourceAddress().String() != "203.0.113.9" || iph.DestinationAddress().String() != "10.99.0.2" {
			t.Fatalf("reply2 ip src=%s dst=%s", iph.SourceAddress(), iph.DestinationAddress())
		}
		msg := iph.Payload()
		if header.ICMPv4(msg).Type() != header.ICMPv4EchoReply || header.ICMPv4(msg).Ident() != 0x1234 {
			t.Fatalf("reply2 icmp type=%d ident=%#x", header.ICMPv4(msg).Type(), header.ICMPv4(msg).Ident())
		}
		if !testICMPChecksumValid(msg) {
			t.Fatal("reply2 checksum invalid")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no relayed reply2")
	}
}

// Non-echo ICMP types and malformed packets must never create sessions
// and must not panic.
func TestHandleICMPPacketDropsNonEcho(t *testing.T) {
	u := newUserspaceBackend()
	if err := u.setup(nil, nil); err != nil {
		t.Fatal(err)
	}
	defer u.cleanup(nil, nil)

	u.handleICMPPacket(testICMPPacket(t, "10.99.0.2", "203.0.113.9", header.ICMPv4DstUnreachable, 0, 0, nil), tcpip.FullAddress{NIC: 1})
	u.handleICMPPacket(testICMPPacket(t, "10.99.0.2", "203.0.113.9", header.ICMPv4EchoReply, 0x1234, 1, []byte("x")), tcpip.FullAddress{NIC: 1})
	u.handleICMPPacket([]byte{0x45, 0x00}, tcpip.FullAddress{NIC: 1})
	u.handleICMPPacket(nil, tcpip.FullAddress{NIC: 1})
	if u.icmpTable.len() != 0 {
		t.Fatalf("non-echo packets created %d sessions", u.icmpTable.len())
	}
}

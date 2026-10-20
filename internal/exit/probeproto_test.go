package exit

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// testUDPPacket builds a checksummed IPv4+UDP packet.
func testUDPPacket(t *testing.T, src string, srcPort uint16, dst string, dstPort uint16, payload []byte) []byte {
	t.Helper()
	udpLen := header.UDPMinimumSize + len(payload)
	pkt := make([]byte, header.IPv4MinimumSize+udpLen)
	pkt[0] = 0x45 // version 4, IHL 5
	pkt[9] = byte(header.UDPProtocolNumber)
	ip := header.IPv4(pkt)
	ip.SetHeaderLength(header.IPv4MinimumSize)
	ip.SetTotalLength(uint16(len(pkt)))
	ip.SetTTL(64)
	ip.SetSourceAddress(testV4Addr(t, src))
	ip.SetDestinationAddress(testV4Addr(t, dst))
	ip.SetChecksum(^ip.CalculateChecksum())
	uh := header.UDP(pkt[header.IPv4MinimumSize:])
	uh.SetSourcePort(srcPort)
	uh.SetDestinationPort(dstPort)
	uh.SetLength(uint16(udpLen))
	copy(pkt[header.IPv4MinimumSize+header.UDPMinimumSize:], payload)
	xsum := header.PseudoHeaderChecksum(header.UDPProtocolNumber, testV4Addr(t, src), testV4Addr(t, dst), uint16(udpLen))
	xsum = checksum.Combine(xsum, checksum.Checksum(payload, 0))
	uh.SetChecksum(^uh.CalculateChecksum(xsum))
	return pkt
}

func validProbeRequest() []byte {
	return append(append([]byte{}, udpProbeMagic[:]...), bytes.Repeat([]byte{0xab}, udpProbeNonceSize)...)
}

func TestUDPProbeReply(t *testing.T) {
	req := validProbeRequest()
	reply := udpProbeReply(req)
	if !bytes.Equal(reply, req) {
		t.Fatalf("valid request not echoed: % x", reply)
	}
	reply[0] = 0
	if req[0] != 'P' {
		t.Fatal("reply aliases the request buffer")
	}

	badMagic := append([]byte("XVMP"), req[4:]...)
	if udpProbeReply(badMagic) != nil {
		t.Fatal("bad magic accepted")
	}
	if udpProbeReply(req[:udpProbeMsgSize-1]) != nil {
		t.Fatal("short request accepted")
	}
	if udpProbeReply(append(append([]byte{}, req...), 0)) != nil {
		t.Fatal("long request accepted")
	}
	if udpProbeReply(nil) != nil {
		t.Fatal("empty request accepted")
	}
}

// The netfilter-path listener echoes valid probes, ignores garbage and
// oversized datagrams, and a concurrent Close terminates the loop.
func TestUDPProbeListener(t *testing.T) {
	ln, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	go udpProbeListener(ln)

	conn, err := net.DialUDP("udp", nil, ln.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	req := validProbeRequest()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write(req); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 128)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("probe reply: %v", err)
	}
	if !bytes.Equal(buf[:n], req) {
		t.Fatalf("reply = % x, want % x", buf[:n], req)
	}

	// garbage and oversized datagrams get no answer
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	big := append(append([]byte{}, req...), bytes.Repeat([]byte{1}, 100)...)
	if _, err := conn.Write(big); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if n, err := conn.Read(buf); err == nil {
		t.Fatalf("garbage answered: % x", buf[:n])
	}

	// concurrent close: requests in flight while the listener shuts down
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = conn.Write(req)
				time.Sleep(time.Millisecond)
			}
		}
	}()
	time.Sleep(50 * time.Millisecond)
	ln.Close()
	close(stop)
	wg.Wait()
	ln.Close() // udpProbeListener's deferred close; must not panic
}

// The userspace-path intercept answers a valid probe at packet level through
// the gVisor endpoint and drops malformed datagrams without touching the
// session table.
func TestUDPProbeUserspaceInterceptPacket(t *testing.T) {
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
	u.InterceptUDP("10.99.0.1:18082", serveUDPProbeConn)

	req := validProbeRequest()
	ct.inbound <- testUDPPacket(t, "10.99.0.2", 40000, "10.99.0.1", UDPProbePort, req)
	select {
	case out := <-ct.outbound:
		iph := header.IPv4(out)
		if iph.SourceAddress().String() != "10.99.0.1" || iph.DestinationAddress().String() != "10.99.0.2" {
			t.Fatalf("reply ip src=%s dst=%s", iph.SourceAddress(), iph.DestinationAddress())
		}
		if got := header.UDP(iph.Payload()).Payload(); !bytes.Equal(got, req) {
			t.Fatalf("reply payload = % x, want % x", got, req)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no probe reply")
	}

	ct.inbound <- testUDPPacket(t, "10.99.0.2", 40001, "10.99.0.1", UDPProbePort, []byte("junk"))
	select {
	case out := <-ct.outbound:
		t.Fatalf("malformed probe answered: % x", out)
	case <-time.After(300 * time.Millisecond):
	}
	if u.sessions.len() != 0 {
		t.Fatal("intercept must not consume the udp session table")
	}
}

// Echo to a locally-registered tunnel address is answered inside the stack:
// swapped addresses, echo-reply type, valid checksum, same ident/seq/payload,
// and no host ping socket (no relay session).
func TestICMPLocalAnswerThroughStack(t *testing.T) {
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
	u.AnswerICMPLocal("10.99.0.1")

	ct.inbound <- testICMPPacket(t, "10.99.0.2", "10.99.0.1", header.ICMPv4Echo, 0x1234, 9, []byte("pvm"))
	select {
	case out := <-ct.outbound:
		iph := header.IPv4(out)
		if iph.SourceAddress().String() != "10.99.0.1" || iph.DestinationAddress().String() != "10.99.0.2" {
			t.Fatalf("reply ip src=%s dst=%s", iph.SourceAddress(), iph.DestinationAddress())
		}
		msg := iph.Payload()
		mh := header.ICMPv4(msg)
		if mh.Type() != header.ICMPv4EchoReply || mh.Ident() != 0x1234 || mh.Sequence() != 9 {
			t.Fatalf("reply icmp type=%d ident=%#x seq=%d", mh.Type(), mh.Ident(), mh.Sequence())
		}
		if !testICMPChecksumValid(msg) {
			t.Fatal("reply checksum invalid")
		}
		if string(msg[header.ICMPv4MinimumSize:]) != "pvm" {
			t.Fatalf("reply payload = %q", msg[header.ICMPv4MinimumSize:])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no local echo reply")
	}
	if u.icmpTable.len() != 0 {
		t.Fatal("local answer must not create a relay session")
	}
}

// Full path: WG handshake against a real userspace server, then ProbeUDP and
// ProbeICMP to the tunnel-side .1, both transports.
func TestProbeUDPAndICMPUserspaceEndToEnd(t *testing.T) {
	for _, transport := range []string{"udp", "tcp"} {
		t.Run(transport, func(t *testing.T) {
			srv := startUserspaceServer(t, transport)
			entry := enrollViaTOFU(t, srv, transport)
			serverIP := "10.99.0.1"
			if transport == "tcp" {
				serverIP = "10.100.0.1"
			}
			tun, err := DialEntry(entry)
			if err != nil {
				t.Fatal(err)
			}
			defer tun.Close()
			ctx, cancel := context.WithTimeout(context.Background(), testLongTimeout)
			defer cancel()
			if err := tun.WaitHandshake(ctx, serverIP); err != nil {
				t.Fatal(err)
			}
			sip := netip.MustParseAddr(serverIP)
			if err := tun.ProbeUDP(ctx, sip); err != nil {
				t.Fatalf("ProbeUDP: %v", err)
			}
			if err := tun.ProbeICMP(ctx, sip); err != nil {
				t.Fatalf("ProbeICMP: %v", err)
			}
		})
	}
}

// Failure modes through a real tunnel: corrupted nonce, wrong source
// address, oversized reply, and no reply at all (UDP and ICMP).
func TestProbeFailureModes(t *testing.T) {
	srv, nets := inProcessServer(t, "udp")
	entry := enrollViaTOFU(t, srv, "udp")
	tun, err := DialEntry(entry)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()
	ctx, cancel := context.WithTimeout(context.Background(), testLongTimeout)
	defer cancel()
	if err := tun.WaitHandshake(ctx, "10.99.0.1"); err != nil {
		t.Fatal(err)
	}
	serverNet := nets["pvmexit0"]
	sip := netip.MustParseAddr("10.99.0.1")
	dst := netip.AddrPortFrom(sip, UDPProbePort)

	oldUDP, oldICMP := udpProbeTimeout, icmpProbeTimeout
	udpProbeTimeout, icmpProbeTimeout = testProbeTimeout, testProbeTimeout
	defer func() { udpProbeTimeout, icmpProbeTimeout = oldUDP, oldICMP }()

	t.Run("wrong nonce", func(t *testing.T) {
		ln, err := serverNet.ListenUDPAddrPort(dst)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		// answer every (re)transmission with a corrupted nonce
		go func() {
			buf := make([]byte, 128)
			for {
				_ = ln.SetReadDeadline(time.Now().Add(30 * time.Second))
				n, src, err := ln.ReadFrom(buf)
				if err != nil {
					return
				}
				buf[n-1] ^= 0xff
				if _, err := ln.WriteTo(buf[:n], src); err != nil {
					return
				}
			}
		}()
		err = tun.ProbeUDP(ctx, sip)
		if err == nil || !strings.Contains(err.Error(), "nonce") {
			t.Fatalf("want nonce mismatch, got %v", err)
		}
	})

	t.Run("wrong source", func(t *testing.T) {
		ln, err := serverNet.ListenUDPAddrPort(dst)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		other, err := serverNet.ListenUDPAddrPort(netip.AddrPortFrom(sip, 0))
		if err != nil {
			t.Fatal(err)
		}
		defer other.Close()
		// answer every (re)transmission from the wrong source port
		go func() {
			buf := make([]byte, 128)
			for {
				_ = ln.SetReadDeadline(time.Now().Add(30 * time.Second))
				n, src, err := ln.ReadFrom(buf)
				if err != nil {
					return
				}
				if _, err := other.WriteTo(buf[:n], src); err != nil {
					return
				}
			}
		}()
		err = tun.ProbeUDP(ctx, sip)
		if err == nil || !strings.Contains(err.Error(), "want "+dst.String()) {
			t.Fatalf("want wrong-source error, got %v", err)
		}
	})

	t.Run("oversized reply", func(t *testing.T) {
		ln, err := serverNet.ListenUDPAddrPort(dst)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		// answer every (re)transmission with an oversized datagram
		go func() {
			buf := make([]byte, 128)
			for {
				_ = ln.SetReadDeadline(time.Now().Add(30 * time.Second))
				n, src, err := ln.ReadFrom(buf)
				if err != nil {
					return
				}
				if _, err := ln.WriteTo(append(buf[:n:n], bytes.Repeat([]byte{1}, 100)...), src); err != nil {
					return
				}
			}
		}()
		err = tun.ProbeUDP(ctx, sip)
		if err == nil || !strings.Contains(err.Error(), "nonce") {
			t.Fatalf("want nonce mismatch, got %v", err)
		}
	})

	t.Run("udp no reply", func(t *testing.T) {
		ln, err := serverNet.ListenUDPAddrPort(dst)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		// swallow every (re)transmission
		go func() {
			buf := make([]byte, 128)
			for {
				_ = ln.SetReadDeadline(time.Now().Add(30 * time.Second))
				if _, _, err := ln.ReadFrom(buf); err != nil {
					return
				}
			}
		}()
		err = tun.ProbeUDP(ctx, sip)
		if err == nil || !strings.Contains(err.Error(), "no reply") {
			t.Fatalf("want timeout error, got %v", err)
		}
	})

	t.Run("icmp no reply", func(t *testing.T) {
		err := tun.ProbeICMP(ctx, netip.MustParseAddr("10.99.0.254"))
		if err == nil || !strings.Contains(err.Error(), "icmp probe") {
			t.Fatalf("want icmp probe error, got %v", err)
		}
	})
}

func TestCheckICMPProbeReply(t *testing.T) {
	payload := []byte("pvm-icmp-probe\x01\x02")
	echo := func(typ header.ICMPv4Type, seq uint16, pl []byte) []byte {
		msg := make([]byte, header.ICMPv4MinimumSize+len(pl))
		h := header.ICMPv4(msg)
		h.SetType(typ)
		h.SetCode(0)
		h.SetIdent(0x1234)
		h.SetSequence(seq)
		copy(msg[header.ICMPv4MinimumSize:], pl)
		h.SetChecksum(header.ICMPv4Checksum(msg[:header.ICMPv4MinimumSize], checksum.Checksum(pl, 0)))
		return msg
	}
	if err := checkICMPProbeReply(echo(header.ICMPv4EchoReply, 7, payload), 7, payload); err != nil {
		t.Fatalf("valid reply rejected: %v", err)
	}
	if err := checkICMPProbeReply(echo(header.ICMPv4Echo, 7, payload), 7, payload); err == nil {
		t.Fatal("echo request accepted as reply")
	}
	if err := checkICMPProbeReply(echo(header.ICMPv4EchoReply, 8, payload), 7, payload); err == nil {
		t.Fatal("wrong sequence accepted")
	}
	if err := checkICMPProbeReply(echo(header.ICMPv4EchoReply, 7, []byte("other")), 7, payload); err == nil {
		t.Fatal("wrong payload accepted")
	}
	if err := checkICMPProbeReply(echo(header.ICMPv4EchoReply, 7, payload)[:4], 7, payload); err == nil {
		t.Fatal("truncated reply accepted")
	}
}

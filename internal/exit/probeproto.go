package exit

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// UDPProbePort is the tunnel-side UDP port of the exit-agent's built-in
// probe (server .1 only). It answers magic+nonce challenges with the exact
// nonce, proving the tunnel carries UDP to the agent itself without any
// third-party service.
const UDPProbePort = 18082

// udpProbeMagic marks datagrams as pvm UDP probes so the listener never
// answers unrelated traffic that happens to hit the port.
var udpProbeMagic = [4]byte{'P', 'V', 'M', 'P'}

const (
	udpProbeNonceSize = 16
	// udpProbeMsgSize is the exact probe request and reply size.
	udpProbeMsgSize = len(udpProbeMagic) + udpProbeNonceSize
	// udpProbeBufSize bounds probe datagram reads; larger requests are
	// truncated and then rejected by the exact-size check.
	udpProbeBufSize = 64
	// udpProbeIdleTimeout closes an intercepted probe conn once the client
	// stops sending (UDP has no close signal).
	udpProbeIdleTimeout = 5 * time.Second
)

// udpProbeTimeout and icmpProbeTimeout bound one probe exchange (relaxed in
// -race test runs).
var (
	udpProbeTimeout  = 10 * time.Second
	icmpProbeTimeout = 10 * time.Second
)

// udpProbeReply returns the exact reply for a well-formed probe request, or
// nil for malformed or oversized datagrams.
func udpProbeReply(req []byte) []byte {
	if len(req) != udpProbeMsgSize || !bytes.Equal(req[:len(udpProbeMagic)], udpProbeMagic[:]) {
		return nil
	}
	reply := make([]byte, udpProbeMsgSize)
	copy(reply, req)
	return reply
}

// udpProbeListener answers UDP probe datagrams on a real tunnel-side
// address (netfilter backends). Closing ln terminates the loop.
func udpProbeListener(ln *net.UDPConn) {
	defer ln.Close()
	buf := make([]byte, udpProbeBufSize)
	for {
		n, src, err := ln.ReadFromUDP(buf)
		if err != nil {
			return // listener closed
		}
		if reply := udpProbeReply(buf[:n]); reply != nil {
			_, _ = ln.WriteToUDP(reply, src)
		}
	}
}

// serveUDPProbeConn answers UDP probe datagrams on an intercepted in-stack
// connection (userspace NAT path, where no host listener exists).
func serveUDPProbeConn(c *gonet.UDPConn) {
	defer c.Close()
	buf := make([]byte, udpProbeBufSize)
	for {
		_ = c.SetReadDeadline(time.Now().Add(udpProbeIdleTimeout))
		n, err := c.Read(buf)
		if err != nil {
			return
		}
		reply := udpProbeReply(buf[:n])
		if reply == nil {
			continue
		}
		if _, err := c.Write(reply); err != nil {
			return
		}
	}
}

// probeExchange resends the probe until check accepts a reply or the
// deadline (min of ctx and timeout) passes; per-attempt reads are capped so
// a lost packet triggers a resend instead of burning the whole budget.
func probeExchange(ctx context.Context, timeout time.Duration, what string,
	write func() error,
	read func(deadline time.Time) ([]byte, error),
	check func([]byte) error) error {
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	for {
		if err := write(); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		attempt := time.Now().Add(2 * time.Second)
		if deadline.Before(attempt) {
			attempt = deadline
		}
		reply, err := read(attempt)
		if err == nil {
			return check(reply)
		}
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			return fmt.Errorf("%s: read: %w", what, err)
		}
		if ctx.Err() != nil {
			return fmt.Errorf("%s: no reply: %w", what, ctx.Err())
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%s: no reply within %s (%w)", what, timeout, err)
		}
	}
}

// ProbeUDP challenges the exit-agent's own probe on the tunnel-side
// serverIP:UDPProbePort through the WireGuard netstack and verifies the
// exact echoed reply, its source address, and the nonce.
func (t *Tunnel) ProbeUDP(ctx context.Context, serverIP netip.Addr) error {
	if !serverIP.Is4() {
		return fmt.Errorf("udp probe: server ip %s is not ipv4", serverIP)
	}
	// bind the tunnel address: the wildcard bind fails route resolution on
	// write in this netstack
	pc, err := t.Net.ListenUDPAddrPort(netip.AddrPortFrom(t.localIP, 0))
	if err != nil {
		return fmt.Errorf("udp probe: listen: %w", err)
	}
	defer pc.Close()
	req := make([]byte, udpProbeMsgSize)
	copy(req, udpProbeMagic[:])
	if _, err := rand.Read(req[len(udpProbeMagic):]); err != nil {
		return fmt.Errorf("udp probe: nonce: %w", err)
	}
	dst := netip.AddrPortFrom(serverIP, UDPProbePort)
	dstAddr := &net.UDPAddr{IP: net.IP(dst.Addr().AsSlice()), Port: int(dst.Port())}
	buf := make([]byte, udpProbeBufSize)
	var from netip.AddrPort
	return probeExchange(ctx, udpProbeTimeout, "udp probe",
		func() error {
			_, err := pc.WriteTo(req, dstAddr)
			return err
		},
		func(deadline time.Time) ([]byte, error) {
			_ = pc.SetReadDeadline(deadline)
			n, src, err := pc.ReadFrom(buf)
			if err != nil {
				return nil, err
			}
			ua, ok := src.(*net.UDPAddr)
			if !ok {
				return nil, fmt.Errorf("unexpected reply address %T", src)
			}
			ip, ok := netip.AddrFromSlice(ua.IP)
			if !ok {
				return nil, fmt.Errorf("unexpected reply ip %q", ua.IP)
			}
			from = netip.AddrPortFrom(ip.Unmap(), uint16(ua.Port))
			return buf[:n], nil
		},
		func(reply []byte) error {
			if from != dst {
				return fmt.Errorf("udp probe: reply from %s, want %s", from, dst)
			}
			if !bytes.Equal(reply, req) {
				return fmt.Errorf("udp probe: reply from %s does not echo the nonce", from)
			}
			return nil
		})
}

// icmpProbePayloadMagic marks probe payloads so a stray echo reply can never
// be mistaken for ours.
var icmpProbePayloadMagic = []byte("pvm-icmp-probe")

// checkICMPProbeReply validates one echo reply: correct type, sequence, and
// the exact payload we sent.
func checkICMPProbeReply(reply []byte, seq uint16, payload []byte) error {
	if len(reply) < header.ICMPv4MinimumSize {
		return fmt.Errorf("icmp probe: short reply (%d bytes)", len(reply))
	}
	h := header.ICMPv4(reply)
	if h.Type() != header.ICMPv4EchoReply {
		return fmt.Errorf("icmp probe: reply type %d, want echo reply", h.Type())
	}
	if h.Sequence() != seq {
		return fmt.Errorf("icmp probe: reply seq %d, want %d", h.Sequence(), seq)
	}
	if !bytes.Equal(reply[header.ICMPv4MinimumSize:], payload) {
		return fmt.Errorf("icmp probe: payload mismatch")
	}
	return nil
}

// ProbeICMP pings the exit-agent's tunnel-side serverIP through the
// WireGuard netstack and verifies the echo reply's type, sequence, and
// payload. Netfilter agents answer on the tunnel NIC itself; userspace
// agents answer inside their NAT stack. No third party is involved.
func (t *Tunnel) ProbeICMP(ctx context.Context, serverIP netip.Addr) error {
	if !serverIP.Is4() {
		return fmt.Errorf("icmp probe: server ip %s is not ipv4", serverIP)
	}
	pc, err := t.Net.DialPingAddr(netip.Addr{}, serverIP)
	if err != nil {
		return fmt.Errorf("icmp probe: dial: %w", err)
	}
	defer pc.Close()
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("icmp probe: nonce: %w", err)
	}
	payload := append(append([]byte{}, icmpProbePayloadMagic...), nonce...)
	seq := binary.BigEndian.Uint16(nonce[:2])
	msg := make([]byte, header.ICMPv4MinimumSize+len(payload))
	h := header.ICMPv4(msg)
	h.SetType(header.ICMPv4Echo)
	h.SetCode(0)
	h.SetIdent(0) // the stack's icmp endpoint rewrites ident and the checksum
	h.SetSequence(seq)
	copy(msg[header.ICMPv4MinimumSize:], payload)
	h.SetChecksum(header.ICMPv4Checksum(msg[:header.ICMPv4MinimumSize], checksum.Checksum(payload, 0)))
	buf := make([]byte, header.ICMPv4MinimumSize+len(payload)+16)
	return probeExchange(ctx, icmpProbeTimeout, "icmp probe",
		func() error {
			_, err := pc.Write(msg)
			return err
		},
		func(deadline time.Time) ([]byte, error) {
			_ = pc.SetReadDeadline(deadline)
			n, err := pc.Read(buf)
			return buf[:n], err
		},
		func(reply []byte) error {
			return checkICMPProbeReply(reply, seq, payload)
		})
}

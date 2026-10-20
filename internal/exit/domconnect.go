package exit

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Domain-connect is the in-tunnel "dial this domain for me" channel: the
// gateway sends a domain and port, the exit resolves and dials them from
// the VPS (the gateway performs no DNS resolution for proxied domains at
// all). cmd 2 is the resolve-only variant used by fake-ip relays and
// real-IP DNS: the exit answers with the egress-permitted IPv4 and closes,
// without a relay.
// Listeners live on each tunnel NIC's server address
// (10.<base>.0.1 / 10.<base+1>.0.1), so only authenticated WG peers can
// reach them.
//
// Request:  [ver:1][cmd:1][domain_len:2 BE][domain][port:2 BE]
//
//	[ver:1][cmd:2][domain_len:2 BE][domain]         (resolve only)
//
// Response: [status:1][msg_len:2 BE][msg]
// status 0 = connected; for cmd 1 the connection then becomes a raw relay
// pipe. For cmd 2 status 0 means resolved and msg carries the IPv4 string;
// the exit closes the connection without relaying.
const (
	DomainConnectPort = 18080

	dcVersion    = 1
	dcCmdConnect = 1
	dcCmdResolve = 2

	dcMaxDomain = 253
	dcMaxMsg    = 1024

	// dcIdleTimeout closes relays with no traffic in either direction;
	// long-lived proxied connections (SSH etc.) stay up while active.
	dcIdleTimeout = 2 * time.Hour
	// dcMaxPerPeer caps concurrent relays per tunnel peer.
	dcMaxPerPeer = 128
)

// DomainConnectAddr joins the exit's tunnel-side server address (learned
// from the enroll response) with the domain-connect port; "" when
// serverIP is not a valid IP.
func DomainConnectAddr(serverIP string) string {
	if net.ParseIP(serverIP) == nil {
		return ""
	}
	return net.JoinHostPort(serverIP, strconv.Itoa(DomainConnectPort))
}

// WriteDomainConnect performs the client side of the handshake and
// leaves conn ready for relay. Any failure closes nothing; caller owns conn.
func WriteDomainConnect(conn net.Conn, domain string, port int) error {
	if len(domain) == 0 || len(domain) > dcMaxDomain {
		return fmt.Errorf("bad domain length %d", len(domain))
	}
	frame := make([]byte, 0, 2+2+len(domain)+2)
	frame = append(frame, dcVersion, dcCmdConnect, byte(len(domain)>>8), byte(len(domain)))
	frame = append(frame, domain...)
	frame = append(frame, byte(port>>8), byte(port))
	if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		return err
	}
	if _, err := conn.Write(frame); err != nil {
		return fmt.Errorf("write connect: %w", err)
	}
	hdr := make([]byte, 3)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return fmt.Errorf("read status: %w", err)
	}
	mlen := int(binary.BigEndian.Uint16(hdr[1:3]))
	if mlen > dcMaxMsg {
		return errors.New("status message too long")
	}
	msg := make([]byte, mlen)
	if _, err := io.ReadFull(conn, msg); err != nil {
		return fmt.Errorf("read status msg: %w", err)
	}
	if hdr[0] != 0 {
		return fmt.Errorf("exit refused %s: %s", domain, string(msg))
	}
	return conn.SetDeadline(time.Time{})
}

// WriteDomainResolve performs the client side of the resolve handshake:
// it asks the exit to resolve domain from the VPS (no port, no relay) and
// returns the egress-permitted IPv4 address. Any failure closes nothing;
// caller owns conn.
func WriteDomainResolve(conn net.Conn, domain string) (net.IP, error) {
	if len(domain) == 0 || len(domain) > dcMaxDomain {
		return nil, fmt.Errorf("bad domain length %d", len(domain))
	}
	frame := make([]byte, 0, 2+2+len(domain))
	frame = append(frame, dcVersion, dcCmdResolve, byte(len(domain)>>8), byte(len(domain)))
	frame = append(frame, domain...)
	if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		return nil, err
	}
	if _, err := conn.Write(frame); err != nil {
		return nil, fmt.Errorf("write resolve: %w", err)
	}
	hdr := make([]byte, 3)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return nil, fmt.Errorf("read status: %w", err)
	}
	mlen := int(binary.BigEndian.Uint16(hdr[1:3]))
	if mlen > dcMaxMsg {
		return nil, errors.New("status message too long")
	}
	msg := make([]byte, mlen)
	if _, err := io.ReadFull(conn, msg); err != nil {
		return nil, fmt.Errorf("read status msg: %w", err)
	}
	if hdr[0] != 0 {
		return nil, fmt.Errorf("exit refused %s: %s", domain, string(msg))
	}
	ip := net.ParseIP(string(msg)).To4()
	if ip == nil {
		return nil, fmt.Errorf("exit returned bad IPv4 %q", string(msg))
	}
	return ip, conn.SetDeadline(time.Time{})
}

// dcServer is the server side of domain-connect with egress policy
// (no loopback/link-local/RFC1918/metadata unless explicitly allowed),
// per-peer relay caps and idle timeouts. Without these the service is
// an authenticated open proxy able to reach VPS-local services.
type dcServer struct {
	allow []*net.IPNet // extra CIDRs exempt from the private-egress block
	// dial is replaceable in tests; nil uses restrictedDial.
	dial func(network, address string) (net.Conn, error)
	// resolve is replaceable in tests; nil uses restrictedResolve.
	resolve func(domain string) (net.IP, error)

	mu     sync.Mutex
	active map[string]int // peer IP -> active relay count
}

func newDCServer(allow []*net.IPNet) *dcServer {
	return &dcServer{allow: allow, active: map[string]int{}}
}

// serve accepts on ln until it is closed.
func (d *dcServer) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go d.handle(conn)
	}
}

func (d *dcServer) enter(peer string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.active[peer] >= dcMaxPerPeer {
		return false
	}
	d.active[peer]++
	return true
}

func (d *dcServer) leave(peer string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.active[peer] > 0 {
		d.active[peer]--
	}
}

// blockedEgressIP reports whether ip may not be dialed by domain-connect:
// loopback, link-local (incl. cloud metadata 169.254.169.254), RFC1918,
// multicast/reserved and non-IPv4 addresses.
func blockedEgressIP(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return true
	}
	switch {
	case v4[0] == 0 || v4[0] >= 224:
		return true
	case v4[0] == 127:
		return true
	case v4[0] == 169 && v4[1] == 254:
		return true
	case v4[0] == 10:
		return true
	case v4[0] == 172 && v4[1]&0xf0 == 16:
		return true
	case v4[0] == 192 && v4[1] == 168:
		return true
	}
	return false
}

// validConnectDomain allows only DNS-shaped names, never IP literals
// (an IP literal would skip the resolve-and-check dial path's domain
// semantics; private-IP destinations are blocked at dial time anyway).
func validConnectDomain(d string) bool {
	if len(d) == 0 || len(d) > dcMaxDomain || net.ParseIP(d) != nil {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(d, "."), ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			ok := c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_'
			if !ok {
				return false
			}
		}
	}
	return true
}

// restrictedDial refuses blocked destinations at connect time (after
// resolution, before the socket connects — no DNS-rebinding window),
// unless the address is inside an operator-allowed CIDR.
func (d *dcServer) restrictedDial(network, address string) (net.Conn, error) {
	dialer := &net.Dialer{
		Timeout: 15 * time.Second,
		Control: func(_, addr string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil {
				return fmt.Errorf("unresolved dial address %s", host)
			}
			for _, n := range d.allow {
				if n.Contains(ip) {
					return nil
				}
			}
			if blockedEgressIP(ip) {
				return fmt.Errorf("egress to %s refused by exit policy", ip)
			}
			return nil
		},
	}
	return dialer.Dial(network, address)
}

// restrictedResolve resolves domain with the VPS's own resolver and
// returns the first egress-permitted IPv4 under the same policy as the
// dial path, so a resolved-but-private answer can never leak into a relay.
func (d *dcServer) restrictedResolve(domain string) (net.IP, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", domain)
	if err != nil {
		return nil, err
	}
	ip, err := d.pickResolveIP(ips)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", domain, err)
	}
	return ip, nil
}

// pickResolveIP returns the first resolved IPv4 allowed by the egress
// policy; allow CIDRs exempt an address from the private-egress block.
func (d *dcServer) pickResolveIP(ips []net.IP) (net.IP, error) {
	for _, ip := range ips {
		v4 := ip.To4()
		if v4 == nil {
			continue
		}
		blocked := blockedEgressIP(v4)
		for _, n := range d.allow {
			if n.Contains(v4) {
				blocked = false
				break
			}
		}
		if !blocked {
			return v4, nil
		}
	}
	return nil, errors.New("no public IPv4 permitted by exit policy")
}

func (d *dcServer) handle(conn net.Conn) {
	defer conn.Close()
	peer := "unknown"
	if a, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
		peer = a.IP.String()
	}
	if !d.enter(peer) {
		log.Printf("domain-connect: too many relays from %s, refusing", peer)
		return
	}
	defer d.leave(peer)

	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	reply := func(status byte, msg string) {
		if len(msg) > dcMaxMsg {
			msg = msg[:dcMaxMsg]
		}
		out := append([]byte{status, byte(len(msg) >> 8), byte(len(msg))}, msg...)
		_, _ = conn.Write(out)
	}
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return
	}
	if hdr[0] != dcVersion || hdr[1] != dcCmdConnect && hdr[1] != dcCmdResolve {
		reply(1, "bad version/cmd")
		return
	}
	dlen := int(binary.BigEndian.Uint16(hdr[2:4]))
	if dlen == 0 || dlen > dcMaxDomain {
		reply(1, "bad domain length")
		return
	}
	if hdr[1] == dcCmdResolve {
		buf := make([]byte, dlen)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return
		}
		domain := strings.ToLower(string(buf))
		if !validConnectDomain(domain) {
			reply(1, "invalid or disallowed domain")
			return
		}
		resolveFn := d.resolve
		if resolveFn == nil {
			resolveFn = d.restrictedResolve
		}
		ip, err := resolveFn(domain)
		if err != nil {
			reply(1, err.Error())
			return
		}
		reply(0, ip.String())
		return
	}
	buf := make([]byte, dlen+2)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return
	}
	domain := strings.ToLower(string(buf[:dlen]))
	port := int(binary.BigEndian.Uint16(buf[dlen:]))
	if !validConnectDomain(domain) {
		reply(1, "invalid or disallowed domain")
		return
	}
	dialFn := d.dial
	if dialFn == nil {
		dialFn = d.restrictedDial
	}
	up, err := dialFn("tcp", net.JoinHostPort(domain, strconv.Itoa(port)))
	if err != nil {
		reply(1, err.Error())
		return
	}
	defer up.Close()
	reply(0, "")
	log.Printf("domain-connect: %s:%d relay established for %s", domain, port, peer)
	proxyTCP(
		&idleTimeoutConn{Conn: conn, d: dcIdleTimeout},
		&idleTimeoutConn{Conn: up, d: dcIdleTimeout},
	)
}

// idleTimeoutConn refreshes the connection deadline on every read/write,
// so relays die only after a full idle period with no traffic.
type idleTimeoutConn struct {
	net.Conn
	d time.Duration
}

func (c *idleTimeoutConn) bump() { _ = c.Conn.SetDeadline(time.Now().Add(c.d)) }

func (c *idleTimeoutConn) Read(p []byte) (int, error) {
	c.bump()
	return c.Conn.Read(p)
}

func (c *idleTimeoutConn) Write(p []byte) (int, error) {
	c.bump()
	return c.Conn.Write(p)
}

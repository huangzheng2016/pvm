package exit

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"time"
)

// Domain-connect is the in-tunnel "dial this domain for me" channel: the
// gateway sends a domain and port, the exit resolves and dials them from
// the VPS (the gateway performs no DNS resolution for proxied domains at
// all). Listeners live on each tunnel NIC's server address
// (10.<base>.<t>.1), so only authenticated WG peers can reach them.
//
// Request:  [ver:1][cmd:1][domain_len:2 BE][domain][port:2 BE]
// Response: [status:1][msg_len:2 BE][msg]
// status 0 = connected; the connection then becomes a raw relay pipe.
const (
	DomainConnectPort = 18080

	dcVersion    = 1
	dcCmdConnect = 1

	dcMaxDomain = 253
	dcMaxMsg    = 1024
)

// DomainConnectAddr maps a client tunnel IP (10.<base>.<t>.<n>) to the
// domain-connect address on the same NIC (.1 of its /24).
func DomainConnectAddr(tunnelIP string) string {
	ip := net.ParseIP(tunnelIP)
	if ip == nil {
		return ""
	}
	v4 := ip.To4()
	if v4 == nil {
		return ""
	}
	v4[3] = 1
	return net.JoinHostPort(v4.String(), strconv.Itoa(DomainConnectPort))
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

// serveDomainConnect accepts on ln until Close; dialFn may be nil (then
// plain net.Dial to the VPS's egress is used).
func serveDomainConnect(ln net.Listener, dialFn func(network, address string) (net.Conn, error)) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			handleDomainConn(conn, dialFn)
		}()
	}
}

// handleDomainConn parses one connect frame, dials, answers and relays.
func handleDomainConn(conn net.Conn, dialFn func(network, address string) (net.Conn, error)) {
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
	if hdr[0] != dcVersion || hdr[1] != dcCmdConnect {
		reply(1, "bad version/cmd")
		return
	}
	dlen := int(binary.BigEndian.Uint16(hdr[2:4]))
	if dlen == 0 || dlen > dcMaxDomain {
		reply(1, "bad domain length")
		return
	}
	buf := make([]byte, dlen+2)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return
	}
	domain := string(buf[:dlen])
	port := int(binary.BigEndian.Uint16(buf[dlen:]))
	addr := net.JoinHostPort(domain, strconv.Itoa(port))
	if dialFn == nil {
		dialFn = func(network, address string) (net.Conn, error) {
			return net.DialTimeout(network, address, 15*time.Second)
		}
	}
	up, err := dialFn("tcp", addr)
	if err != nil {
		reply(1, err.Error())
		return
	}
	reply(0, "")
	_ = conn.SetDeadline(time.Time{})
	_ = up.SetDeadline(time.Time{})
	log.Printf("domain-connect: %s relay established", addr)
	proxyTCP(conn, up)
}

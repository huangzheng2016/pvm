package gateway

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"syscall"
)

// DHCPServer is a minimal DHCPv4 server: fixed MAC->IP assignments from
// the daemon-pushed config, broadcast replies, no dynamic pool.
type DHCPServer struct {
	mu      sync.RWMutex
	byMAC   map[string]VMEntry
	bindDev string

	conn *net.UDPConn
	gwIP net.IP
	mask net.IPMask
	dns  net.IP
}

func NewDHCPServer(byMAC map[string]VMEntry, bindDev string) *DHCPServer {
	s := &DHCPServer{bindDev: bindDev}
	s.UpdateMap(byMAC)
	return s
}

func (s *DHCPServer) UpdateMap(byMAC map[string]VMEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byMAC = byMAC
	for _, v := range byMAC { // derive gw/mask/dns from the first /24 entry
		ip := net.ParseIP(v.IP).To4()
		if ip == nil {
			continue
		}
		gw := net.IPv4(ip[0], ip[1], ip[2], 1)
		s.gwIP = gw
		s.mask = net.CIDRMask(24, 32)
		s.dns = gw
		break
	}
}

func (s *DHCPServer) Start() error {
	lc := net.ListenConfig{
		Control: func(_, _ string, c syscall.RawConn) error {
			var err error
			c.Control(func(fd uintptr) {
				if err = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1); err != nil {
					return
				}
				// broadcast replies otherwise follow the default route
				// out the uplink and never reach the cluster segment.
				// SO_BINDTODEVICE is linux-only (=25); the agent only
				// ever runs on linux guests, and the constant keeps the
				// package testable on darwin.
				err = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, 25, s.bindDev)
			})
			return err
		},
	}
	pc, err := lc.ListenPacket(context.Background(), "udp4", ":67")
	if err != nil {
		return err
	}
	conn := pc.(*net.UDPConn)
	s.conn = conn
	go s.serve()
	return nil
}

func (s *DHCPServer) serve() {
	buf := make([]byte, 1500)
	for {
		n, _, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("dhcp: handle panic: %v", r)
				}
			}()
			s.handle(pkt)
		}()
	}
}

func (s *DHCPServer) handle(pkt []byte) {
	req, err := parseDHCP(pkt)
	if err != nil {
		return
	}
	s.mu.RLock()
	entry, known := s.byMAC[req.mac]
	gw, mask, dns := s.gwIP, s.mask, s.dns
	s.mu.RUnlock()
	if !known || gw == nil {
		log.Printf("dhcp: msgtype %d from unknown mac %s, ignored", req.msgType, req.mac)
		return
	}
	ip := net.ParseIP(entry.IP).To4()
	if ip == nil {
		return
	}
	var respType byte
	switch req.msgType {
	case 1: // DISCOVER
		respType = 2 // OFFER
	case 3: // REQUEST
		respType = 5 // ACK
	default:
		return
	}
	resp := buildDHCPReply(req, respType, ip, gw, mask, dns, entry.Name)
	if _, err := s.conn.WriteToUDP(resp, &net.UDPAddr{IP: net.IPv4bcast, Port: 68}); err != nil {
		log.Printf("dhcp reply to %s: %v", entry.Name, err)
		return
	}
	log.Printf("dhcp: %s -> %s (%s %s)", req.mac, entry.IP, entry.Name, dhcpTypeName(respType))
}

func dhcpTypeName(t byte) string {
	switch t {
	case 2:
		return "OFFER"
	case 5:
		return "ACK"
	}
	return fmt.Sprintf("type%d", t)
}

type dhcpRequest struct {
	xid     uint32
	mac     string
	msgType byte
	flags   uint16
}

func parseDHCP(b []byte) (*dhcpRequest, error) {
	if len(b) < 240 {
		return nil, fmt.Errorf("short dhcp packet %d", len(b))
	}
	if b[0] != 1 { // BOOTREQUEST
		return nil, fmt.Errorf("not a bootrequest")
	}
	if b[1] != 1 || int(b[2]) > 16 || len(b) < 28+int(b[2]) {
		return nil, fmt.Errorf("bad htype/hlen %d/%d", b[1], b[2])
	}
	if !equalBytes(b[236:240], []byte{99, 130, 83, 99}) {
		return nil, fmt.Errorf("bad magic cookie")
	}
	r := &dhcpRequest{
		xid:   binary.BigEndian.Uint32(b[4:8]),
		flags: binary.BigEndian.Uint16(b[10:12]),
	}
	r.mac = formatMAC(b[28 : 28+int(b[2])])
	for i := 240; i+2 <= len(b); {
		code, ln := b[i], int(b[i+1])
		if code == 255 {
			break
		}
		if code == 0 || i+2+ln > len(b) {
			i++
			continue
		}
		if code == 53 && ln >= 1 {
			r.msgType = b[i+2]
		}
		i += 2 + ln
	}
	if r.msgType == 0 {
		return nil, fmt.Errorf("no msg type option")
	}
	return r, nil
}

func buildDHCPReply(req *dhcpRequest, msgType byte, yiaddr, gw net.IP, mask net.IPMask, dns net.IP, hostname string) []byte {
	out := make([]byte, 548)
	out[0] = 2 // BOOTREPLY
	out[1], out[2], out[3] = 1, 6, 0
	binary.BigEndian.PutUint32(out[4:8], req.xid)
	binary.BigEndian.PutUint16(out[10:12], req.flags)
	copy(out[16:20], yiaddr.To4())
	copy(out[20:24], gw.To4()) // siaddr
	copy(out[28:44], mustParseMAC(req.mac))
	copy(out[236:240], []byte{99, 130, 83, 99})

	var opts []byte
	add := func(code byte, val ...byte) {
		opts = append(opts, code, byte(len(val)))
		opts = append(opts, val...)
	}
	add(53, msgType)
	add(54, gw.To4()...)          // server id
	add(51, 0, 0, 0x8c, 0xa0)     // lease 6h
	add(1, net.IP(mask).To4()...) // subnet mask
	add(3, gw.To4()...)           // router
	add(6, dns.To4()...)          // dns
	if hostname != "" {
		add(12, []byte(hostname)...)
	}
	opts = append(opts, 255)
	n := copy(out[240:], opts)
	return out[:240+n]
}

func formatMAC(b []byte) string {
	parts := make([]string, len(b))
	for i, x := range b {
		parts[i] = fmt.Sprintf("%02x", x)
	}
	return strings.Join(parts, ":")
}

func mustParseMAC(s string) []byte {
	m, _ := net.ParseMAC(s)
	return m
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

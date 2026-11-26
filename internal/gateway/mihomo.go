package gateway

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"time"
)

// socks5Dial opens a TCP stream through a local Mihomo SOCKS5 listener using
// ATYP=3 (domain name) so the selected exit resolves the name remotely.
func socks5Dial(ctx context.Context, serverAddr, domain string, port int) (net.Conn, error) {
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp4", serverAddr)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(10 * time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)
	if err := socks5Handshake(conn, domain, port); err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func socks5Handshake(conn net.Conn, domain string, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("invalid target port %d", port)
	}
	var req []byte
	if ip := net.ParseIP(domain); ip != nil && ip.To4() != nil {
		req = []byte{5, 1, 0, 1}
		req = append(req, ip.To4()...)
	} else {
		if len(domain) == 0 || len(domain) > 255 {
			return fmt.Errorf("invalid target domain length %d", len(domain))
		}
		req = append([]byte{5, 1, 0, 3, byte(len(domain))}, domain...)
	}
	req = append(req, byte(port>>8), byte(port))
	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		return err
	}
	var method [2]byte
	if _, err := io.ReadFull(conn, method[:]); err != nil {
		return err
	}
	if method[0] != 5 || method[1] != 0 {
		return fmt.Errorf("socks5 auth method rejected (0x%x)", method[1])
	}
	if _, err := conn.Write(req); err != nil {
		return err
	}
	var hdr [4]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return err
	}
	if hdr[0] != 5 || hdr[1] != 0 {
		return fmt.Errorf("socks5 connect failed (rep=0x%x)", hdr[1])
	}
	return discardSocks5BoundAddr(conn, hdr[3])
}

func discardSocks5BoundAddr(conn net.Conn, atyp byte) error {
	var n int
	switch atyp {
	case 1:
		n = 4 + 2
	case 4:
		n = 16 + 2
	case 3:
		var l [1]byte
		if _, err := io.ReadFull(conn, l[:]); err != nil {
			return err
		}
		n = int(l[0]) + 2
	default:
		return fmt.Errorf("invalid socks5 atyp 0x%x", atyp)
	}
	_, err := io.CopyN(io.Discard, conn, int64(n))
	return err
}

func (m *exitManager) probeMihomo() bool {
	m.mu.Lock()
	ports := make(map[string]int, len(m.mihomoPorts))
	for k, v := range m.mihomoPorts {
		ports[k] = v
	}
	was := make(map[string]bool, len(m.mihomoUp))
	for k, v := range m.mihomoUp {
		was[k] = v
	}
	m.mu.Unlock()

	changed := false
	for name, port := range ports {
		conn, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
		up := err == nil
		if conn != nil {
			conn.Close()
		}
		if was[name] != up {
			changed = true
			if !up {
				log.Printf("exit %s: mihomo listener 127.0.0.1:%d unreachable; failing closed", name, port)
			}
		}
		m.mu.Lock()
		m.mihomoUp[name] = up
		m.mu.Unlock()
	}
	return changed
}

// MihomoPortFor returns the gateway-local SOCKS5 port when the Mihomo exit is healthy.
func (m *exitManager) MihomoPortFor(name string) (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	port := m.mihomoPorts[name]
	return port, port > 0 && m.mihomoUp[name]
}

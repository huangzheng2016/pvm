package exit

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestDomainConnectAddr(t *testing.T) {
	got := DomainConnectAddr("10.100.0.1")
	if got != "10.100.0.1:18080" {
		t.Fatalf("DomainConnectAddr = %q", got)
	}
	if DomainConnectAddr("not-an-ip") != "" {
		t.Fatal("bad ip must yield empty addr")
	}
	if DomainConnectAddr("") != "" {
		t.Fatal("empty server ip must yield empty addr")
	}
}

func startTestDCServer(t *testing.T, dialErr error) (string, chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	dialed := make(chan string, 4)
	dialFn := func(network, address string) (net.Conn, error) {
		dialed <- address
		if dialErr != nil {
			return nil, dialErr
		}
		// loop the connection back to an echo pipe
		c1, c2 := net.Pipe()
		go func() { _, _ = io.Copy(c2, c2) }() // echo
		return c1, nil
	}
	dc := newDCServer(nil)
	dc.dial = dialFn
	go dc.serve(ln)
	return ln.Addr().String(), dialed
}

func TestDomainConnectRoundtrip(t *testing.T) {
	addr, dialed := startTestDCServer(t, nil)
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := WriteDomainConnect(conn, "example.com", 443); err != nil {
		t.Fatalf("WriteDomainConnect: %v", err)
	}
	select {
	case got := <-dialed:
		if got != "example.com:443" {
			t.Fatalf("dial target = %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("dialFn never called")
	}
	// relay works both ways after the handshake
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("relay read: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("relay got %q", buf)
	}
}

func TestDomainConnectRefused(t *testing.T) {
	addr, _ := startTestDCServer(t, io.ErrClosedPipe)
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	err = WriteDomainConnect(conn, "no-such-host.invalid", 80)
	if err == nil || !strings.Contains(err.Error(), "exit refused") {
		t.Fatalf("want refusal, got %v", err)
	}
}

func TestDomainConnectBadVersion(t *testing.T) {
	addr, dialed := startTestDCServer(t, nil)
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte{9, 1, 0, 3, 'a', '.', 'b', 0, 80}); err != nil {
		t.Fatal(err)
	}
	resp := make([]byte, 3+16)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, _ := conn.Read(resp)
	if n < 3 || resp[0] == 0 {
		t.Fatalf("want error status, got %v", resp[:n])
	}
	select {
	case <-dialed:
		t.Fatal("dialFn must not run on bad version")
	case <-time.After(300 * time.Millisecond):
	}
}

func startTestDCResolveServer(t *testing.T, resolve func(string) (net.IP, error)) (string, chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	queried := make(chan string, 4)
	dc := newDCServer(nil)
	dc.resolve = func(domain string) (net.IP, error) {
		queried <- domain
		return resolve(domain)
	}
	go dc.serve(ln)
	return ln.Addr().String(), queried
}

func TestDomainResolveRoundtrip(t *testing.T) {
	addr, queried := startTestDCResolveServer(t, func(domain string) (net.IP, error) {
		return net.ParseIP("203.0.113.10"), nil
	})
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ip, err := WriteDomainResolve(conn, "example.com")
	if err != nil {
		t.Fatalf("WriteDomainResolve: %v", err)
	}
	if len(ip) != 4 || !ip.Equal(net.ParseIP("203.0.113.10")) {
		t.Fatalf("resolve ip = %v, want 203.0.113.10", ip)
	}
	select {
	case got := <-queried:
		if got != "example.com" {
			t.Fatalf("resolve target = %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("resolve never called")
	}
	// resolve must not become a relay: the exit closes after replying.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("resolve conn must close without relay, got %v", err)
	}
}

func TestDomainResolveFailure(t *testing.T) {
	addr, _ := startTestDCResolveServer(t, func(domain string) (net.IP, error) {
		return nil, errors.New("no such host")
	})
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = WriteDomainResolve(conn, "no-such-host.invalid")
	if err == nil || !strings.Contains(err.Error(), "exit refused") ||
		!strings.Contains(err.Error(), "no such host") {
		t.Fatalf("want resolve failure, got %v", err)
	}
}

func TestDomainResolvePrivateRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	dc := newDCServer(nil)
	// The resolver answers with private addresses only; the default
	// egress policy must reject them all.
	dc.resolve = func(domain string) (net.IP, error) {
		return dc.pickResolveIP([]net.IP{net.ParseIP("10.0.0.7"), net.ParseIP("192.168.1.1")})
	}
	go dc.serve(ln)

	conn, err := net.DialTimeout("tcp", ln.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = WriteDomainResolve(conn, "internal.example")
	if err == nil || !strings.Contains(err.Error(), "exit refused") {
		t.Fatalf("want private-address refusal, got %v", err)
	}
}

func TestDomainResolveUnknownCommand(t *testing.T) {
	addr, queried := startTestDCResolveServer(t, func(domain string) (net.IP, error) {
		return net.ParseIP("203.0.113.10"), nil
	})
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte{1, 9, 0, 3, 'a', '.', 'b'}); err != nil {
		t.Fatal(err)
	}
	hdr := make([]byte, 3)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, hdr); err != nil {
		t.Fatal(err)
	}
	msg := make([]byte, binary.BigEndian.Uint16(hdr[1:3]))
	if _, err := io.ReadFull(conn, msg); err != nil {
		t.Fatal(err)
	}
	if hdr[0] == 0 || string(msg) != "bad version/cmd" {
		t.Fatalf("want bad version/cmd, got status %d msg %q", hdr[0], msg)
	}
	select {
	case <-queried:
		t.Fatal("resolve must not run on unknown command")
	case <-time.After(300 * time.Millisecond):
	}
}

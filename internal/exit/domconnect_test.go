package exit

import (
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

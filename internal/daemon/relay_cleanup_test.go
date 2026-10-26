package daemon

import (
	"net"
	"testing"
	"time"
)

func TestTCPRelayRemoveClosesEstablishedConnections(t *testing.T) {
	backend, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	r := newRelaySet()
	if err := r.Ensure("tcp", 0, backend.Addr().(*net.TCPAddr).Port); err != nil {
		t.Fatal(err)
	}
	defer r.Remove("tcp", 0)
	src, err := net.Dial("tcp", r.tcp[relayKey("tcp", 0)].Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	backend.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second))
	dst, err := backend.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if _, err := src.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	dst.SetReadDeadline(time.Now().Add(5 * time.Second))
	var buf [1]byte
	if _, err := dst.Read(buf[:]); err != nil {
		t.Fatal(err)
	}
	r.Remove("tcp", 0)
	for _, c := range []net.Conn{src, dst} {
		c.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := c.Read(buf[:]); err == nil {
			t.Fatal("relay connection survived removal")
		} else if e, ok := err.(net.Error); ok && e.Timeout() {
			t.Fatal("relay connection remained open")
		}
	}
}

func TestUDPRelayCloseClosesSessions(t *testing.T) {
	backend, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	rel, err := startUDPRelay(0, backend.LocalAddr().(*net.UDPAddr).Port)
	if err != nil {
		t.Fatal(err)
	}
	defer rel.close()
	src := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 18080}
	rel.dispatch(src, []byte("x"))
	rel.mu.Lock()
	sess := rel.sessions[src.String()]
	rel.mu.Unlock()
	if sess == nil {
		t.Fatal("session missing")
	}
	rel.close()
	if _, err := sess.dst.Write([]byte("x")); err == nil {
		t.Fatal("session socket stayed open")
	}
	rel.dispatch(src, []byte("x"))
	rel.mu.Lock()
	defer rel.mu.Unlock()
	if len(rel.sessions) != 0 {
		t.Fatal("closed relay retained or reopened sessions")
	}
}

package tunnel

import (
	"net"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/conn"
)

func openTestBind(t *testing.T) (*TCPBind, string) {
	t.Helper()
	b := NewTCPBind().(*TCPBind)
	_, port, err := b.Open(0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b, (&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(port)}).String()
}

func TestTCPBindFirstReadTimeout(t *testing.T) {
	orig := tcpFirstReadTimeoutNs.Load()
	tcpFirstReadTimeoutNs.Store(int64(150 * time.Millisecond))
	defer func() { tcpFirstReadTimeoutNs.Store(orig) }()

	_, addr := openTestBind(t)
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	one := make([]byte, 1)
	if _, err := c.Read(one); err == nil {
		t.Fatal("idle connection must be closed after the first-read deadline")
	}
}

// Per-source-IP and total connection caps refuse floods and free slots
// when conns close.
func TestTCPBindPerIPCap(t *testing.T) {
	orig := tcpFirstReadTimeoutNs.Load()
	tcpFirstReadTimeoutNs.Store(int64(time.Hour)) // isolate the cap from the idle deadline
	defer func() { tcpFirstReadTimeoutNs.Store(orig) }()

	_, addr := openTestBind(t)
	var held []net.Conn
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()
	dial := func() net.Conn {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		return c
	}
	for i := 0; i < tcpMaxConnsPerIP; i++ {
		held = append(held, dial())
	}
	// give the accept loop a moment to register all conns
	time.Sleep(200 * time.Millisecond)

	over := dial()
	defer over.Close()
	over.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := over.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection beyond the per-IP cap must be refused")
	}

	// freeing a slot lets a new conn in
	held[0].Close()
	held = held[1:]
	time.Sleep(200 * time.Millisecond)
	c := dial()
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("fresh conn after freeing a slot was closed early")
	}
}

// dialLock entries are removed after use so the map cannot grow forever.
func TestTCPBindDialingMapCleaned(t *testing.T) {
	b := NewTCPBind().(*TCPBind)
	fns, _, err := b.Open(0)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	_ = fns
	ep, err := b.ParseEndpoint("127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := b.getConn(ep); err == nil {
			t.Fatal("dial to closed port must fail")
		}
	}
	b.mu.Lock()
	n := len(b.dialing)
	b.mu.Unlock()
	if n != 0 {
		t.Fatalf("dialing map leaked %d entries", n)
	}
}

var _ = conn.ErrWrongEndpointType

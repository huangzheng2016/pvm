package gateway

import (
	"net"
	"testing"
	"time"
)

// dialProxy must fail closed: without a live tunnel mark or a reachable
// domain-connect address the function returns immediately without
// dialing anything (the caller, handle, then closes the source).
func TestDialProxyFailClosed(t *testing.T) {
	// case 1: tunnel down (markFor = -1)
	src := tcpPair(t)
	p := newFakeProxy(nil, "10.0.2.3", nil)
	done := make(chan struct{})
	go func() { p.dialProxy(src, "example.com", 443, "my-exit", ""); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("mark=-1 must refuse fast")
	}

	// case 2: tunnel up but domain-connect unavailable
	src2 := tcpPair(t)
	p2 := newFakeProxy(nil, "10.0.2.3", nil)
	p2.markFor = func(string) int { return 0x99 }
	done2 := make(chan struct{})
	go func() { p2.dialProxy(src2, "example.com", 443, "my-exit", ""); close(done2) }()
	select {
	case <-done2:
	case <-time.After(2 * time.Second):
		t.Fatal("missing domain-connect addr must refuse fast")
	}
}

// tcpPair returns one end of a real TCP connection (other end drained).
func tcpPair(t *testing.T) *net.TCPConn {
	t.Helper()
	ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				buf := make([]byte, 1024)
				for {
					if _, err := c.Read(buf); err != nil {
						c.Close()
						return
					}
				}
			}()
		}
	}()
	conn, err := net.DialTCP("tcp4", nil, ln.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

package stream

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// echoAddr is a loopback TCP echo server; real targets are TCP or unix
// sockets, which support half-close.
var echoAddr = func() string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				io.Copy(c, c)
				c.(*net.TCPConn).CloseWrite()
			}()
		}
	}()
	return ln.Addr().String()
}()

// echoDial serves "echo" and refuses other targets.
func echoDial(target string) (io.ReadWriteCloser, error) {
	if target != "echo" {
		return nil, fmt.Errorf("target %q not allowed", target)
	}
	return net.Dial("tcp", echoAddr)
}

// pair connects a host session to a guest Serve over an in-memory pipe.
// junk is written to the guest first, as left over from an old session.
func pair(t *testing.T, junk []byte) *Session {
	t.Helper()
	h, g := net.Pipe()
	served := make(chan error, 1)
	go func() {
		served <- Serve(g, echoDial)
	}()
	if len(junk) > 0 {
		// net.Pipe writes complete only once the guest has read them
		if _, err := h.Write(junk); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := Client(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.Close()
		select {
		case <-served:
		case <-time.After(5 * time.Second):
			t.Error("guest session did not end after the host closed")
		}
	})
	return s
}

func TestEchoRoundTrip(t *testing.T) {
	s := pair(t, []byte("stale bytes PVMSTR without the full magic"))
	ctx := context.Background()
	st, err := s.Open(ctx, "echo")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
		msg := []byte(fmt.Sprintf("message %d", i))
		if _, err := st.Write(msg); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(msg))
		if _, err := io.ReadFull(st, got); err != nil || !bytes.Equal(got, msg) {
			t.Fatalf("got %q, %v; want %q", got, err, msg)
		}
	}
	if _, err := s.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	st.CloseWrite()
	if n, err := io.Copy(io.Discard, st); n != 0 || err != nil {
		t.Fatalf("after half-close: read %d, %v; want clean EOF", n, err)
	}
}

// More than the window must flow both ways at once without stalling.
func TestBulkConcurrentStreams(t *testing.T) {
	s := pair(t, nil)
	const size = 4 << 20
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := s.Open(context.Background(), "echo")
			if err != nil {
				errs <- err
				return
			}
			data := make([]byte, size)
			rand.Read(data)
			go func() {
				st.Write(data)
				st.CloseWrite()
			}()
			got, err := io.ReadAll(st)
			if err != nil || !bytes.Equal(got, data) {
				errs <- fmt.Errorf("echoed %d bytes, %v; want %d identical", len(got), err, size)
			}
			st.Close()
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("bulk transfer stalled")
	}
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestOpenRefusedTarget(t *testing.T) {
	s := pair(t, nil)
	if _, err := s.Open(context.Background(), "../../etc/passwd"); err == nil {
		t.Fatal("guest accepted a target outside its allowlist")
	}
	// the session survives a refused open
	if _, err := s.Open(context.Background(), "echo"); err != nil {
		t.Fatal(err)
	}
}

func TestGuestCannotOpen(t *testing.T) {
	h, g := net.Pipe()
	go func() {
		// guest side, played by hand: ack the hello, then try to open
		r := make([]byte, headerLen+len(magic)+8)
		io.ReadFull(g, r)
		ack := append(encodeHeader(frameHelloAck, 0, len(magic)+8), r[headerLen:]...)
		g.Write(ack)
		open := append(encodeHeader(frameOpen, 7, 4), "echo"...)
		g.Write(open)
		resp := make([]byte, headerLen)
		io.ReadFull(g, resp)
		if resp[0] != frameReset {
			t.Errorf("host answered a guest open with frame %d, want reset", resp[0])
		}
		g.Close()
	}()
	s, err := Client(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	<-s.Done()
}

func TestSessionCloseFailsStreams(t *testing.T) {
	s := pair(t, nil)
	st, err := s.Open(context.Background(), "echo")
	if err != nil {
		t.Fatal(err)
	}
	read := make(chan error, 1)
	go func() {
		_, err := st.Read(make([]byte, 1))
		read <- err
	}()
	s.Close()
	select {
	case err := <-read:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("read after session close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read blocked after the session closed")
	}
}

func TestHandshakeTimeoutWithoutGuest(t *testing.T) {
	h, g := net.Pipe()
	go io.Copy(io.Discard, g)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if _, err := Client(ctx, h); err == nil {
		t.Fatal("handshake succeeded with no guest")
	}
}

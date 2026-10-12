package tunnel

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/conn"
)

func TestTCPBindRoundTrip(t *testing.T) {
	bind := NewTCPBind().(*TCPBind)
	fns, port, err := bind.Open(0)
	if err != nil {
		t.Fatal(err)
	}
	defer bind.Close()
	if len(fns) != 1 {
		t.Fatalf("want 1 receive func, got %d", len(fns))
	}

	c, err := net.DialTCP("tcp", nil, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(port)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	payload := []byte("hello-wireguard")
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := c.Write(append(hdr[:], payload...)); err != nil {
		t.Fatal(err)
	}

	bufs := [][]byte{make([]byte, MaxSegmentSize)}
	sizes := make([]int, 1)
	eps := make([]conn.Endpoint, 1)
	n, err := fns[0](bufs, sizes, eps)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || sizes[0] != len(payload) || string(bufs[0][:sizes[0]]) != string(payload) {
		t.Fatalf("bad receive: n=%d size=%d data=%q", n, sizes[0], bufs[0][:sizes[0]])
	}
	if eps[0] == nil {
		t.Fatal("endpoint not set")
	}

	// Send back through the bind to the endpoint we just received from.
	reply := []byte("pong")
	if err := bind.Send([][]byte{reply}, eps[0]); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	var rhdr [4]byte
	if _, err := io.ReadFull(c, rhdr[:]); err != nil {
		t.Fatal(err)
	}
	rlen := binary.LittleEndian.Uint32(rhdr[:])
	got := make([]byte, rlen)
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(reply) {
		t.Fatalf("bad reply: %q", got)
	}
}

func TestTCPBindRejectsOversizedFrame(t *testing.T) {
	bind := NewTCPBind().(*TCPBind)
	_, port, err := bind.Open(0)
	if err != nil {
		t.Fatal(err)
	}
	defer bind.Close()

	c, err := net.DialTCP("tcp", nil, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(port)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], MaxSegmentSize+1)
	if _, err := c.Write(hdr[:]); err != nil {
		t.Fatal(err)
	}
	// Server should tear the connection down.
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err = c.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("expected connection close after oversized frame")
	}
}

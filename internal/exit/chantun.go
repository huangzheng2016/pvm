package exit

import (
	"net"
	"os"
	"sync"

	"golang.zx2c4.com/wireguard/tun"
)

// chanTUN is an in-memory tun.Device bridging a WireGuard device to the
// userspace NAT: packets the WG device decrypts (Write) go to gVisor,
// packets gVisor emits (Read) get encrypted back to the client.
type chanTUN struct {
	name     string
	mtu      int
	inbound  chan []byte // WG device -> gVisor
	outbound chan []byte // gVisor -> WG device
	events   chan tun.Event
	closed   chan struct{}
	once     sync.Once
}

func newChanTUN(name string, mtu int) *chanTUN {
	c := &chanTUN{
		name:     name,
		mtu:      mtu,
		inbound:  make(chan []byte, 4096),
		outbound: make(chan []byte, 4096),
		events:   make(chan tun.Event, 1),
		closed:   make(chan struct{}),
	}
	c.events <- tun.EventUp
	return c
}

func (c *chanTUN) Name() (string, error) { return c.name, nil }
func (c *chanTUN) MTU() (int, error)     { return c.mtu, nil }
func (c *chanTUN) BatchSize() int        { return 1 }
func (c *chanTUN) Events() <-chan tun.Event {
	return c.events
}

func (c *chanTUN) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	select {
	case pkt := <-c.outbound:
		n := copy(bufs[0][offset:], pkt)
		sizes[0] = n
		return 1, nil
	case <-c.closed:
		return 0, net.ErrClosed
	}
}

func (c *chanTUN) Write(bufs [][]byte, offset int) (int, error) {
	for _, b := range bufs {
		pkt := make([]byte, len(b)-offset)
		copy(pkt, b[offset:])
		select {
		case c.inbound <- pkt:
		case <-c.closed:
			return 0, net.ErrClosed
		}
	}
	return len(bufs), nil
}

// File is unsupported; wireguard-go only needs it for FD-based devices.
func (c *chanTUN) File() *os.File { return nil }

func (c *chanTUN) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

var _ tun.Device = (*chanTUN)(nil)

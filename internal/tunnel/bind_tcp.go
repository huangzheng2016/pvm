// TCP transport for WireGuard: 4-byte little-endian length-prefixed frames
// over persistent TCP connections. Adapted from ecorplink's
// wireguard-go/conn/bind_tcp.go (MIT License,
// Copyright (c) huangzheng2016 — https://github.com/huangzheng2016/ecorplink).
package tunnel

import (
	"fmt"
	"io"
	"net"
	"net/netip"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"golang.zx2c4.com/wireguard/conn"
)

// MaxSegmentSize ref: device.MaxSegmentSize
const MaxSegmentSize = 65535

var _ conn.Bind = (*TCPBind)(nil)

func NewTCPBind() conn.Bind {
	return &TCPBind{
		dataPool: sync.Pool{
			New: func() any {
				return &recvData{buff: make([]byte, MaxSegmentSize)}
			},
		},
	}
}

type TCPBind struct {
	mu         sync.Mutex
	tcpConnMap map[string]*tcpConn
	perIP      map[string]int
	dialing    map[string]*sync.Mutex
	listener   *net.TCPListener
	closed     bool
	acceptOnly bool
	fwmark     uint32

	dataPool  sync.Pool
	recvChan  chan *recvData
	closeChan chan struct{}
}

// Inbound connections are unauthenticated until a WireGuard handshake
// completes, so every idle TCP conn would otherwise cost a 64KB buffer
// and a goroutine forever.
const (
	tcpMaxConns        = 1024
	tcpMaxConnsPerIP   = 16
	tcpKeepAlivePeriod = 30 * time.Second
	tcpWriteTimeout    = 30 * time.Second
)

// tcpFirstReadTimeout is the deadline for the first frame (a WG
// handshake initiation); atomic so tests can shrink it safely.
var tcpFirstReadTimeoutNs = atomic.Int64{}

func firstReadTimeout() time.Duration { return time.Duration(tcpFirstReadTimeoutNs.Load()) }

func init() { tcpFirstReadTimeoutNs.Store(int64(10 * time.Second)) }

type tcpConn struct {
	conn    *net.TCPConn
	writeMu sync.Mutex
}

type reqLen [4]byte

func (l *reqLen) Len() int {
	return int(l[0]) + int(l[1])<<8 + int(l[2])<<16 + int(l[3])<<24
}

func (l *reqLen) FromLen(n int) {
	l[0] = byte(n & 0xff)
	l[1] = byte(n >> 8 & 0xff)
	l[2] = byte(n >> 16 & 0xff)
	l[3] = byte(n >> 24 & 0xff)
}

type recvData struct {
	len      [4]byte
	buff     []byte
	size     int
	endpoint conn.Endpoint
}

func (t *TCPBind) makeReceive() conn.ReceiveFunc {
	closeChan := t.closeChan
	return func(bufs [][]byte, sizes []int, eps []conn.Endpoint) (n int, err error) {
		if len(bufs) == 0 {
			return 0, nil
		}
		count := 0
		select {
		case <-closeChan:
			return 0, net.ErrClosed
		case data := <-t.recvChan:
			count = t.copyReceivedData(bufs, sizes, eps, count, data)
		}
		for count < len(bufs) {
			select {
			case data := <-t.recvChan:
				count = t.copyReceivedData(bufs, sizes, eps, count, data)
			default:
				return count, nil
			}
		}
		return count, nil
	}
}

func (t *TCPBind) copyReceivedData(bufs [][]byte, sizes []int, eps []conn.Endpoint, count int, data *recvData) int {
	if data == nil {
		return count
	}
	sizes[count] = data.size
	copy(bufs[count], data.buff[:sizes[count]])
	eps[count] = data.endpoint
	// The buffer returns to the pool only after copying; handleConn hands a
	// fresh buffer per packet so in-flight data is never overwritten.
	t.dataPool.Put(data)
	return count + 1
}

func (t *TCPBind) handleConn(entry *tcpConn, endpoint conn.Endpoint, closeChan chan struct{}) {
	go func() {
		defer func() {
			t.deleteConn(endpoint.DstToString(), entry)
			entry.conn.Close()
		}()
		// the first frame (a WG handshake initiation) must arrive quickly;
		// afterwards the deadline is lifted and WG keepalives rule.
		entry.conn.SetReadDeadline(time.Now().Add(firstReadTimeout())) //nolint:errcheck
		first := true
		for {
			data := t.dataPool.Get().(*recvData)
			if _, err := io.ReadFull(entry.conn, data.len[:]); err != nil {
				t.dataPool.Put(data)
				return
			}
			l := reqLen(data.len)
			size := l.Len()
			// A malformed/desynced frame length means the stream is
			// unrecoverable; tear it down instead of panicking on the slice.
			if size < 0 || size > len(data.buff) {
				t.dataPool.Put(data)
				return
			}
			n, err := io.ReadFull(entry.conn, data.buff[:size])
			if err != nil {
				t.dataPool.Put(data)
				return
			}
			if n != size {
				t.dataPool.Put(data)
				continue
			}
			if first {
				entry.conn.SetReadDeadline(time.Time{}) //nolint:errcheck
				first = false
			}
			data.size = size
			data.endpoint = endpoint
			select {
			case <-closeChan:
				t.dataPool.Put(data)
				return
			case t.recvChan <- data:
			}
		}
	}()
}

func (t *TCPBind) deleteConn(endpoint string, entry *tcpConn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tcpConnMap != nil && t.tcpConnMap[endpoint] == entry {
		delete(t.tcpConnMap, endpoint)
		if ip, _, err := net.SplitHostPort(endpoint); err == nil && t.perIP[ip] > 0 {
			t.perIP[ip]--
		}
	}
}

func (t *TCPBind) accept(listener *net.TCPListener, closeChan chan struct{}) {
	backoff := 10 * time.Millisecond
	for {
		c, err := listener.AcceptTCP()
		if err != nil {
			if t.isClosed() {
				return
			}
			// EMFILE and friends are temporary; never let the accept loop
			// die silently, the whole TCP transport depends on it.
			select {
			case <-time.After(backoff):
			case <-closeChan:
				return
			}
			if backoff < time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = 10 * time.Millisecond
		c.SetNoDelay(true)                       //nolint:errcheck
		c.SetKeepAlive(true)                     //nolint:errcheck
		c.SetKeepAlivePeriod(tcpKeepAlivePeriod) //nolint:errcheck
		endpoint := &conn.StdNetEndpoint{AddrPort: c.RemoteAddr().(*net.TCPAddr).AddrPort()}
		entry := &tcpConn{conn: c}
		if !t.storeConn(endpoint.DstToString(), entry) {
			continue
		}
		t.handleConn(entry, endpoint, closeChan)
	}
}

func (t *TCPBind) Open(port uint16) ([]conn.ReceiveFunc, uint16, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.recvChan = make(chan *recvData, 1024)
	t.closeChan = make(chan struct{})
	t.tcpConnMap = make(map[string]*tcpConn)
	t.perIP = make(map[string]int)
	t.dialing = make(map[string]*sync.Mutex)
	t.closed = false
	t.acceptOnly = port != 0

	var err error
	t.listener, err = net.ListenTCP("tcp", &net.TCPAddr{Port: int(port)})
	if err != nil {
		return nil, 0, err
	}
	go t.accept(t.listener, t.closeChan)
	fn := t.makeReceive()
	actualPort := uint16(t.listener.Addr().(*net.TCPAddr).Port)
	return []conn.ReceiveFunc{fn}, actualPort, nil
}

func (t *TCPBind) Close() error {
	var err error
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	conns := make([]*tcpConn, 0, len(t.tcpConnMap))
	for _, entry := range t.tcpConnMap {
		conns = append(conns, entry)
	}
	t.tcpConnMap = nil
	listener := t.listener
	t.listener = nil
	closeChan := t.closeChan
	t.closeChan = nil
	t.mu.Unlock()

	for _, entry := range conns {
		if e := entry.conn.Close(); e != nil {
			err = e
		}
	}
	if listener != nil {
		_ = listener.Close()
	}
	if closeChan != nil {
		close(closeChan)
	}
	return err
}

func (t *TCPBind) storeConn(endpoint string, entry *tcpConn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		entry.conn.Close()
		return false
	}
	if t.tcpConnMap == nil {
		t.tcpConnMap = make(map[string]*tcpConn)
	}
	ip, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		ip = endpoint
	}
	if len(t.tcpConnMap) >= tcpMaxConns || t.perIP[ip] >= tcpMaxConnsPerIP {
		entry.conn.Close()
		return false
	}
	if old := t.tcpConnMap[endpoint]; old != nil && old != entry {
		old.conn.Close()
		t.perIP[ip]--
	}
	t.tcpConnMap[endpoint] = entry
	t.perIP[ip]++
	return true
}

func (t *TCPBind) getConn(endpoint conn.Endpoint) (*tcpConn, error) {
	key := endpoint.DstToString()
	if c, ok := t.loadConn(key); ok {
		return c, nil
	}
	t.mu.Lock()
	acceptOnly := t.acceptOnly
	t.mu.Unlock()
	if acceptOnly {
		return nil, fmt.Errorf("tcp connection to %s is not established", key)
	}

	lock := t.dialLock(key)
	lock.Lock()
	defer func() {
		lock.Unlock()
		// drop the entry so the map cannot grow without bound; a late
		// duplicate dial is handled by storeConn's replace logic.
		t.mu.Lock()
		if t.dialing[key] == lock {
			delete(t.dialing, key)
		}
		t.mu.Unlock()
	}()
	if t.isClosed() {
		return nil, net.ErrClosed
	}
	if c, ok := t.loadConn(key); ok {
		return c, nil
	}

	se, ok := endpoint.(*conn.StdNetEndpoint)
	if !ok {
		return nil, conn.ErrWrongEndpointType
	}
	addr := &net.TCPAddr{
		IP:   se.DstIP().AsSlice(),
		Port: int(se.Port()),
	}
	raw, err := net.DialTimeout("tcp", addr.String(), 15*time.Second)
	if err != nil {
		return nil, err
	}
	rawTCP := raw.(*net.TCPConn)
	// Nagle off: the two-write frame (header+payload) would otherwise cost one
	// RTT per WireGuard packet.
	rawTCP.SetNoDelay(true)                       //nolint:errcheck
	rawTCP.SetKeepAlive(true)                     //nolint:errcheck
	rawTCP.SetKeepAlivePeriod(tcpKeepAlivePeriod) //nolint:errcheck
	entry := &tcpConn{conn: rawTCP}
	if !t.storeConn(key, entry) {
		return nil, fmt.Errorf("too many tcp connections")
	}
	t.mu.Lock()
	closeChan := t.closeChan
	t.mu.Unlock()
	if closeChan == nil {
		t.deleteConn(key, entry)
		entry.conn.Close()
		return nil, net.ErrClosed
	}
	t.handleConn(entry, endpoint, closeChan)
	return entry, nil
}

func (t *TCPBind) isClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

func (t *TCPBind) loadConn(endpoint string) (*tcpConn, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.tcpConnMap == nil {
		return nil, false
	}
	c, ok := t.tcpConnMap[endpoint]
	return c, ok
}

func (t *TCPBind) dialLock(endpoint string) *sync.Mutex {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dialing == nil {
		t.dialing = make(map[string]*sync.Mutex)
	}
	lock := t.dialing[endpoint]
	if lock == nil {
		lock = &sync.Mutex{}
		t.dialing[endpoint] = lock
	}
	return lock
}

func (t *TCPBind) Send(bufs [][]byte, endpoint conn.Endpoint) error {
	entry, err := t.getConn(endpoint)
	if err != nil {
		return err
	}
	entry.writeMu.Lock()
	defer entry.writeMu.Unlock()
	for _, buf := range bufs {
		var l reqLen
		l.FromLen(len(buf))
		msg := net.Buffers{l[:], buf}
		entry.conn.SetWriteDeadline(time.Now().Add(tcpWriteTimeout)) //nolint:errcheck
		n, err := msg.WriteTo(entry.conn)
		entry.conn.SetWriteDeadline(time.Time{}) //nolint:errcheck
		if err != nil || n != int64(4+len(buf)) {
			t.deleteConn(endpoint.DstToString(), entry)
			entry.conn.Close()
			if err != nil {
				return err
			}
			return io.ErrShortWrite
		}
	}
	return nil
}

func (t *TCPBind) ParseEndpoint(s string) (conn.Endpoint, error) {
	e, err := netip.ParseAddrPort(s)
	if err != nil {
		return nil, err
	}
	return &conn.StdNetEndpoint{AddrPort: e}, nil
}

func (t *TCPBind) BatchSize() int {
	if runtime.GOOS == "linux" {
		return conn.IdealBatchSize
	}
	return 1
}

func (t *TCPBind) SetMark(mark uint32) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.fwmark = mark
	return nil
}

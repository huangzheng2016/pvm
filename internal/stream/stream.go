// Package stream multiplexes byte streams between the host and a guest
// over the pvm.stream virtio-serial port. Unlike the agent channel it
// carries binary frames with per-stream credit, so a display stream is
// not limited to one frame in flight.
//
// Only the host opens streams, by naming a target from the guest's
// allowlist; the guest cannot reach the host through this channel.
package stream

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// PortName is the virtio-serial port name.
const PortName = "pvm.stream"

const (
	frameHello    = 1 // host -> guest: magic + nonce, starts a session
	frameHelloAck = 2 // guest -> host: echoes magic + nonce
	frameOpen     = 3 // host -> guest: payload is the target name
	frameOpenOK   = 4
	frameOpenErr  = 5 // payload is the reason
	frameData     = 6
	frameCredit   = 7 // payload: uint32 bytes the receiver consumed
	frameClose    = 8 // sender will write no more data
	frameReset    = 9 // abort the stream both ways
	framePing     = 10
	framePong     = 11

	headerLen = 9 // type u8, stream id u32, payload length u32

	// MaxPayload bounds one data frame, so streams interleave fairly.
	MaxPayload = 64 << 10
	// Window is how much a sender may have unacknowledged per stream.
	Window = 256 << 10

	maxTarget = 64
)

// magic marks session start. Bytes left over from an earlier host
// connection are skipped until it appears.
var magic = []byte("PVMSTRM1")

var (
	ErrClosed = errors.New("stream: session closed")
	ErrReset  = errors.New("stream: reset by peer")
)

// Session is one host connection to the guest's pvm.stream port.
type Session struct {
	host bool
	dial func(target string) (io.ReadWriteCloser, error) // guest only

	r   *bufio.Reader
	c   io.Closer
	wmu sync.Mutex
	w   io.Writer

	// control frames written for the read loop, which must never block on
	// a write: if both ends' read loops waited on full write buffers,
	// neither would read again
	ctrlMu   sync.Mutex
	ctrl     [][]byte
	ctrlWake chan struct{}

	mu       sync.Mutex
	streams  map[uint32]*Stream
	nextID   uint32
	pings    map[uint64]chan struct{}
	err      error
	done     chan struct{}
	readDone chan struct{}
}

func newSession(rw io.ReadWriteCloser, r *bufio.Reader, host bool) *Session {
	return &Session{
		host: host, r: r, c: rw, w: rw,
		streams:  make(map[uint32]*Stream),
		pings:    make(map[uint64]chan struct{}),
		done:     make(chan struct{}),
		readDone: make(chan struct{}),
		ctrlWake: make(chan struct{}, 1),
	}
}

// start runs the read and control loops.
func (s *Session) start() {
	go s.ctrlLoop()
	go s.readLoop()
}

// sendAsync queues a control frame from the read loop.
func (s *Session) sendAsync(typ byte, id uint32, payload []byte) {
	s.ctrlMu.Lock()
	s.ctrl = append(s.ctrl, append(encodeHeader(typ, id, len(payload)), payload...))
	s.ctrlMu.Unlock()
	select {
	case s.ctrlWake <- struct{}{}:
	default:
	}
}

func (s *Session) ctrlLoop() {
	for {
		select {
		case <-s.done:
			return
		case <-s.ctrlWake:
		}
		s.ctrlMu.Lock()
		frames := s.ctrl
		s.ctrl = nil
		s.ctrlMu.Unlock()
		for _, f := range frames {
			if err := s.writeRaw(f); err != nil {
				return
			}
		}
	}
}

// Client starts a host session on conn, the host end of the port. It
// sends hellos until the guest echoes one, which skips anything left on
// the channel from an earlier connection.
func Client(ctx context.Context, conn net.Conn) (*Session, error) {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	hello := append(append([]byte{}, magic...), nonce[:]...)
	r := bufio.NewReaderSize(conn, 2*MaxPayload)
	acked := make(chan error, 1)
	go func() { acked <- scanFor(r, hello) }()
	frame := encodeHeader(frameHello, 0, len(hello))
	frame = append(frame, hello...)
	for {
		if _, err := conn.Write(frame); err != nil {
			conn.Close()
			return nil, err
		}
		select {
		case err := <-acked:
			if err != nil {
				conn.Close()
				return nil, fmt.Errorf("stream handshake: %w", err)
			}
			s := newSession(conn, r, true)
			s.start()
			return s, nil
		case <-time.After(time.Second):
		case <-ctx.Done():
			conn.Close()
			<-acked
			return nil, fmt.Errorf("stream handshake: %w (guest agent may not serve %s; restart the VM)", ctx.Err(), PortName)
		}
	}
}

// Serve runs one guest session on rw until the host goes away. dial
// connects an allowed target; anything else must return an error.
func Serve(rw io.ReadWriteCloser, dial func(target string) (io.ReadWriteCloser, error)) error {
	r := bufio.NewReaderSize(rw, 2*MaxPayload)
	if err := scanFor(r, magic); err != nil {
		return err
	}
	var nonce [8]byte
	if _, err := io.ReadFull(r, nonce[:]); err != nil {
		return err
	}
	s := newSession(rw, r, false)
	s.dial = dial
	if err := s.writeFrame(frameHelloAck, 0, append(append([]byte{}, magic...), nonce[:]...)); err != nil {
		return err
	}
	s.start()
	// the next session reads the same port, so wait for this reader
	<-s.readDone
	return s.Err()
}

// scanFor consumes r up to and including the first occurrence of want.
func scanFor(r *bufio.Reader, want []byte) error {
	matched := 0
	for matched < len(want) {
		b, err := r.ReadByte()
		if err != nil {
			return err
		}
		switch {
		case b == want[matched]:
			matched++
		case b == want[0]:
			matched = 1
		default:
			matched = 0
		}
	}
	return nil
}

func encodeHeader(typ byte, id uint32, n int) []byte {
	h := make([]byte, headerLen, headerLen+n)
	h[0] = typ
	binary.BigEndian.PutUint32(h[1:5], id)
	binary.BigEndian.PutUint32(h[5:9], uint32(n))
	return h
}

func (s *Session) writeFrame(typ byte, id uint32, payload []byte) error {
	return s.writeRaw(append(encodeHeader(typ, id, len(payload)), payload...))
}

func (s *Session) writeRaw(frame []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if err := s.Err(); err != nil {
		return err
	}
	if _, err := s.w.Write(frame); err != nil {
		s.fail(err)
		return err
	}
	return nil
}

// Err is the reason the session ended, or nil while it runs.
func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Done is closed when the session ends.
func (s *Session) Done() <-chan struct{} { return s.done }

// Close ends the session and every stream on it.
func (s *Session) Close() error {
	s.fail(ErrClosed)
	return nil
}

func (s *Session) fail(err error) {
	s.mu.Lock()
	if s.err != nil {
		s.mu.Unlock()
		return
	}
	s.err = err
	streams := s.streams
	s.streams = map[uint32]*Stream{}
	for _, ch := range s.pings {
		close(ch)
	}
	s.pings = map[uint64]chan struct{}{}
	s.mu.Unlock()
	close(s.done)
	s.c.Close()
	for _, st := range streams {
		st.abort(err)
	}
}

func (s *Session) readLoop() {
	defer close(s.readDone)
	hdr := make([]byte, headerLen)
	for {
		if _, err := io.ReadFull(s.r, hdr); err != nil {
			s.fail(err)
			return
		}
		typ, id, n := hdr[0], binary.BigEndian.Uint32(hdr[1:5]), binary.BigEndian.Uint32(hdr[5:9])
		if n > MaxPayload {
			s.fail(fmt.Errorf("stream: frame of %d bytes exceeds %d", n, MaxPayload))
			return
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(s.r, payload); err != nil {
			s.fail(err)
			return
		}
		if err := s.handle(typ, id, payload); err != nil {
			s.fail(err)
			return
		}
	}
}

func (s *Session) stream(id uint32) *Stream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streams[id]
}

func (s *Session) handle(typ byte, id uint32, p []byte) error {
	switch typ {
	case frameHello:
		// a repeated hello from the handshake; answer it again
		if s.host || len(p) != len(magic)+8 || !bytes.HasPrefix(p, magic) {
			return fmt.Errorf("stream: unexpected hello")
		}
		s.sendAsync(frameHelloAck, 0, p)
		return nil
	case frameHelloAck:
		if !s.host {
			return fmt.Errorf("stream: unexpected hello ack")
		}
		return nil
	case frameOpen:
		if s.host {
			// the guest may not open streams
			s.sendAsync(frameReset, id, nil)
			return nil
		}
		s.accept(id, string(p))
		return nil
	case frameOpenOK, frameOpenErr:
		if st := s.stream(id); st != nil && st.opened != nil {
			if typ == frameOpenOK {
				st.opened <- nil
			} else {
				st.opened <- fmt.Errorf("guest refused %q: %s", st.target, p)
			}
		}
		return nil
	case frameData:
		st := s.stream(id)
		if st == nil {
			return nil
		}
		return st.deliver(p)
	case frameCredit:
		if len(p) != 4 {
			return fmt.Errorf("stream: bad credit frame")
		}
		if st := s.stream(id); st != nil {
			st.addCredit(int(binary.BigEndian.Uint32(p)))
		}
		return nil
	case frameClose:
		if st := s.stream(id); st != nil {
			st.peerClosed()
		}
		return nil
	case frameReset:
		if st := s.stream(id); st != nil {
			s.remove(id)
			st.abort(ErrReset)
		}
		return nil
	case framePing:
		s.sendAsync(framePong, id, p)
		return nil
	case framePong:
		if len(p) == 8 {
			s.mu.Lock()
			ch := s.pings[binary.BigEndian.Uint64(p)]
			delete(s.pings, binary.BigEndian.Uint64(p))
			s.mu.Unlock()
			if ch != nil {
				close(ch)
			}
		}
		return nil
	}
	return fmt.Errorf("stream: unknown frame type %d", typ)
}

func (s *Session) register(st *Stream) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if _, dup := s.streams[st.id]; dup {
		return fmt.Errorf("stream: duplicate stream id %d", st.id)
	}
	s.streams[st.id] = st
	return nil
}

func (s *Session) remove(id uint32) {
	s.mu.Lock()
	delete(s.streams, id)
	s.mu.Unlock()
}

// Open asks the guest to connect target and returns the stream.
func (s *Session) Open(ctx context.Context, target string) (*Stream, error) {
	if !s.host {
		return nil, fmt.Errorf("stream: only the host opens streams")
	}
	if target == "" || len(target) > maxTarget {
		return nil, fmt.Errorf("stream: invalid target %q", target)
	}
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	s.mu.Unlock()
	st := newStream(s, id, target)
	st.opened = make(chan error, 1)
	if err := s.register(st); err != nil {
		return nil, err
	}
	if err := s.writeFrame(frameOpen, id, []byte(target)); err != nil {
		s.remove(id)
		return nil, err
	}
	select {
	case err := <-st.opened:
		if err != nil {
			s.remove(id)
			return nil, err
		}
		return st, nil
	case <-s.done:
		return nil, s.Err()
	case <-ctx.Done():
		s.remove(id)
		s.writeFrame(frameReset, id, nil)
		return nil, ctx.Err()
	}
}

// Ping measures one round trip through the guest.
func (s *Session) Ping(ctx context.Context) (time.Duration, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	ch := make(chan struct{})
	s.mu.Lock()
	if s.err != nil {
		s.mu.Unlock()
		return 0, s.err
	}
	s.pings[binary.BigEndian.Uint64(b[:])] = ch
	s.mu.Unlock()
	start := time.Now()
	if err := s.writeFrame(framePing, 0, b[:]); err != nil {
		return 0, err
	}
	select {
	case <-ch:
		if err := s.Err(); err != nil {
			return 0, err
		}
		return time.Since(start), nil
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pings, binary.BigEndian.Uint64(b[:]))
		s.mu.Unlock()
		return 0, ctx.Err()
	}
}

// accept connects a guest target for a host Open and pumps it.
func (s *Session) accept(id uint32, target string) {
	st := newStream(s, id, target)
	if err := s.register(st); err != nil {
		s.sendAsync(frameOpenErr, id, []byte(err.Error()))
		return
	}
	go func() {
		conn, err := s.dial(target)
		if err != nil {
			s.remove(id)
			s.writeFrame(frameOpenErr, id, []byte(err.Error()))
			return
		}
		if err := s.writeFrame(frameOpenOK, id, nil); err != nil {
			conn.Close()
			return
		}
		Pipe(st, conn)
	}()
}

// Pipe copies both ways between st and conn, passing half-closes on, and
// closes both when done.
func Pipe(st *Stream, conn io.ReadWriteCloser) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(conn, st)
		closeWrite(conn)
	}()
	go func() {
		defer wg.Done()
		io.Copy(st, conn)
		st.CloseWrite()
	}()
	wg.Wait()
	st.Close()
	conn.Close()
}

func closeWrite(c io.Closer) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
		return
	}
	c.Close()
}

// Stream is one bidirectional byte stream on a session.
type Stream struct {
	s      *Session
	id     uint32
	target string
	opened chan error

	mu         sync.Mutex
	cond       *sync.Cond
	rbuf       bytes.Buffer
	unacked    int  // bytes read but not yet credited back
	peerDone   bool // peer sent Close
	readClosed bool // Close was called locally
	sendCredit int
	wroteClose bool
	err        error
}

func newStream(s *Session, id uint32, target string) *Stream {
	st := &Stream{s: s, id: id, target: target, sendCredit: Window}
	st.cond = sync.NewCond(&st.mu)
	return st
}

func (st *Stream) deliver(p []byte) error {
	st.mu.Lock()
	if st.readClosed || st.err != nil {
		st.mu.Unlock()
		// nobody will read it; return the credit so the peer is not stuck
		st.s.sendAsync(frameCredit, st.id, binary.BigEndian.AppendUint32(nil, uint32(len(p))))
		return nil
	}
	if st.rbuf.Len()+len(p) > Window {
		st.mu.Unlock()
		return fmt.Errorf("stream: peer exceeded the window on stream %d", st.id)
	}
	st.rbuf.Write(p)
	st.cond.Broadcast()
	st.mu.Unlock()
	return nil
}

func (st *Stream) addCredit(n int) {
	st.mu.Lock()
	st.sendCredit += n
	st.cond.Broadcast()
	st.mu.Unlock()
}

func (st *Stream) peerClosed() {
	st.mu.Lock()
	st.peerDone = true
	st.cond.Broadcast()
	done := st.wroteClose
	st.mu.Unlock()
	if done {
		st.s.remove(st.id)
	}
}

func (st *Stream) abort(err error) {
	st.mu.Lock()
	if st.err == nil {
		st.err = err
	}
	st.cond.Broadcast()
	st.mu.Unlock()
	if st.opened != nil {
		select {
		case st.opened <- err:
		default:
		}
	}
}

// Read blocks until data arrives, the peer closes (io.EOF) or the stream
// fails.
func (st *Stream) Read(p []byte) (int, error) {
	st.mu.Lock()
	for st.rbuf.Len() == 0 && !st.peerDone && st.err == nil && !st.readClosed {
		st.cond.Wait()
	}
	if st.rbuf.Len() == 0 {
		err := st.err
		st.mu.Unlock()
		if err == nil {
			err = io.EOF
		}
		return 0, err
	}
	n, _ := st.rbuf.Read(p)
	st.unacked += n
	var credit int
	if st.unacked >= Window/4 || st.rbuf.Len() == 0 {
		credit, st.unacked = st.unacked, 0
	}
	st.mu.Unlock()
	if credit > 0 {
		st.s.writeFrame(frameCredit, st.id, binary.BigEndian.AppendUint32(nil, uint32(credit)))
	}
	return n, nil
}

// Write blocks while the peer's window is full.
func (st *Stream) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		st.mu.Lock()
		for st.sendCredit == 0 && st.err == nil && !st.wroteClose {
			st.cond.Wait()
		}
		if st.err != nil || st.wroteClose {
			err := st.err
			st.mu.Unlock()
			if err == nil {
				err = io.ErrClosedPipe
			}
			return written, err
		}
		n := min(len(p), st.sendCredit, MaxPayload)
		st.sendCredit -= n
		st.mu.Unlock()
		if err := st.s.writeFrame(frameData, st.id, p[:n]); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

// CloseWrite tells the peer no more data follows; reading continues.
func (st *Stream) CloseWrite() error {
	st.mu.Lock()
	if st.wroteClose || st.err != nil {
		st.mu.Unlock()
		return nil
	}
	st.wroteClose = true
	st.cond.Broadcast()
	done := st.peerDone
	st.mu.Unlock()
	err := st.s.writeFrame(frameClose, st.id, nil)
	if done {
		st.s.remove(st.id)
	}
	return err
}

// Close stops reading and writing. Data still arriving is discarded.
func (st *Stream) Close() error {
	st.mu.Lock()
	st.readClosed = true
	st.rbuf.Reset()
	st.cond.Broadcast()
	st.mu.Unlock()
	return st.CloseWrite()
}

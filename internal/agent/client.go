package agent

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// ErrUnreachable marks "agent channel dead" so callers (e.g. pvm ssh)
// can distinguish it from in-guest command failures and fall back.
var ErrUnreachable = errors.New("agent channel unreachable")

// A full 16-request burst must fit even before the workers get scheduled.
const maxQueuedControls = 2 * maxConcurrentControls

type Client struct {
	sock      string
	conn      net.Conn
	w         *frameWriter
	writeGate chan struct{}
	r         *bufio.Reader

	mu        sync.Mutex
	pending   map[string]chan Frame
	control   ControlHandler
	ctrlQueue chan Frame
	busySem   chan struct{}
	ctrlWG    sync.WaitGroup
	closed    bool

	// Per-client timeout overrides (zero = package default); tests
	// shorten them without racing leaked goroutines on shared vars.
	ctrlHandlerTimeout time.Duration
	ctrlWriteTimeout   time.Duration

	done     chan struct{} // closed on connection loss or Close
	doneOnce sync.Once
}

// DialSock connects to the agent channel at a unix socket path.
func DialSock(sock string) (*Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return DialSockContext(ctx, sock)
}

func DialSockContext(ctx context.Context, sock string) (*Client, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", sock)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	c := &Client{
		sock:    sock,
		conn:    conn,
		w:       newFrameWriter(conn),
		r:       bufio.NewReaderSize(conn, 64<<10),
		pending: map[string]chan Frame{},
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

func (c *Client) readLoop() {
	for {
		var f Frame
		if err := readFrame(c.r, &f); err != nil {
			c.failAll(err)
			return
		}
		if f.Op == OpControl {
			// Admission never waits for a handler or a response write. The
			// queue and refusal semaphore bound all control-related work.
			queue, busySem := c.controlState()
			select {
			case queue <- f:
			default:
				select {
				case busySem <- struct{}{}:
					go c.writeControlBusy(&f)
				default:
				}
			}
			continue
		}
		c.mu.Lock()
		key := f.ID
		if f.Op == OpStdinAck {
			key += "/stdin"
		}
		ch := c.pending[key]
		terminal := f.Done || f.Error != "" || f.Op == OpPong
		if terminal {
			delete(c.pending, key)
		}
		overflow := false
		if ch != nil {
			select {
			case ch <- f:
			default:
				overflow = true
			}
		}
		c.mu.Unlock()
		if overflow {
			c.kill(errors.New("agent response flow-control window exceeded"))
			return
		}
	}
}

// HandleControl registers the handler for guest-initiated control
// requests; the host daemon binds it to the VM owning this channel.
// Call before the client is shared.
func (c *Client) HandleControl(h ControlHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.control = h
}

// controlState lazily starts a fixed worker pool (tests construct
// Clients directly).
func (c *Client) controlState() (chan Frame, chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done == nil {
		c.done = make(chan struct{})
	}
	if c.ctrlQueue == nil {
		c.ctrlQueue = make(chan Frame, maxQueuedControls)
		c.ctrlWG.Add(maxConcurrentControls)
		for i := 0; i < maxConcurrentControls; i++ {
			go c.controlWorker(c.ctrlQueue, c.done)
		}
	}
	if c.busySem == nil {
		c.busySem = make(chan struct{}, maxConcurrentControlRefusals)
	}
	return c.ctrlQueue, c.busySem
}

func (c *Client) controlWorker(queue <-chan Frame, done <-chan struct{}) {
	defer c.ctrlWG.Done()
	for {
		select {
		case <-done:
			return
		case f := <-queue:
			select {
			case <-done:
				return
			default:
			}
			c.dispatchControl(&f)
		}
	}
}

func (c *Client) dispatchControl(f *Frame) {
	c.mu.Lock()
	h := c.control
	c.mu.Unlock()
	resp := Frame{ID: f.ID}
	switch {
	case h == nil:
		resp.Error = "control unsupported"
	case len(f.Payload) > MaxControlPayloadBytes:
		resp.Error = "control payload too large"
	default:
		resp.Op, resp.Payload, resp.Error = c.runControl(h, f)
	}
	if err := c.writeWithDeadline(&resp, time.Now().Add(c.writeTimeout())); err != nil {
		c.kill(err)
	}
}

func (c *Client) writeControlBusy(f *Frame) {
	defer func() { <-c.busySem }()
	if err := c.writeWithDeadline(&Frame{ID: f.ID, Error: "control busy"}, time.Now().Add(controlBusyWriteTimeout)); err != nil {
		c.kill(err)
	}
}

func (c *Client) handlerTimeout() time.Duration {
	if c.ctrlHandlerTimeout > 0 {
		return c.ctrlHandlerTimeout
	}
	return controlHandlerTimeout
}

func (c *Client) writeTimeout() time.Duration {
	if c.ctrlWriteTimeout > 0 {
		return c.ctrlWriteTimeout
	}
	return controlWriteTimeout
}

// kill tears the connection down after a failed control write: the
// frameWriter latches the first error, so the channel stays poisoned
// even if the peer resumes reading — closing it marks the client dead
// (waking the daemon's watcher to redial a fresh one) and unblocks
// every pending call.
func (c *Client) kill(err error) {
	c.failAll(err)
	c.conn.Close()
}

// runControl executes the handler under an overall deadline that is
// also cancelled when the connection drops, so abandoned requests stop
// consuming host resources.
func (c *Client) runControl(h ControlHandler, f *Frame) (string, json.RawMessage, string) {
	ctx, cancel := context.WithTimeout(context.Background(), c.handlerTimeout())
	defer cancel()
	if c.done != nil {
		go func() {
			select {
			case <-c.done:
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	payload, err := h(ctx, f)
	switch {
	case err != nil:
		return "", nil, err.Error()
	case len(payload) > MaxControlPayloadBytes:
		return "", nil, "control result too large"
	default:
		return OpResult, payload, ""
	}
}

func (c *Client) signalDone() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.signalDoneLocked()
}

func (c *Client) signalDoneLocked() {
	if c.done == nil {
		c.done = make(chan struct{})
	}
	c.doneOnce.Do(func() { close(c.done) })
}

func (c *Client) failAll(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.signalDoneLocked()
	for id, ch := range c.pending {
		delete(c.pending, id)
		// No sender closes response channels. A terminal error replaces queued
		// data on transport loss and never waits for a consumer.
	drain:
		for {
			select {
			case <-ch:
			default:
				break drain
			}
		}
		ch <- Frame{ID: id, Error: fmt.Sprintf("%v: %v", ErrUnreachable, err)}
	}
}

func (c *Client) write(f *Frame) error {
	return c.writeWithDeadline(f, time.Time{})
}

// writeWithDeadline serializes the frame write and bounds how long it
// may stall on a peer that stopped reading; the deadline is cleared
// afterwards so stream writes are unaffected.
func (c *Client) writeWithDeadline(f *Frame, dl time.Time) error {
	if dl.IsZero() {
		dl = time.Now().Add(30 * time.Second)
	}
	ctx, cancel := context.WithDeadline(context.Background(), dl)
	defer cancel()
	return c.writeContext(ctx, f)
}

func (c *Client) writeContext(ctx context.Context, f *Frame) error {
	c.mu.Lock()
	if c.writeGate == nil {
		c.writeGate = make(chan struct{}, 1)
	}
	gate := c.writeGate
	c.mu.Unlock()
	select {
	case gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-gate }()
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return ErrUnreachable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dl := time.Now().Add(30 * time.Second)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(dl) {
		dl = deadline
	}
	c.conn.SetWriteDeadline(dl)
	cancelled := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { c.conn.SetWriteDeadline(time.Now()); close(cancelled) })
	err := c.w.write(f)
	if !stop() {
		<-cancelled
	}
	c.conn.SetWriteDeadline(time.Time{})
	if err != nil {
		c.kill(err)
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	return nil
}

func (c *Client) Ping() (*Frame, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return c.PingContext(ctx)
}

func (c *Client) PingContext(ctx context.Context) (*Frame, error) {
	id := newID()
	ch := c.register(id)
	defer c.unregister(id)
	if err := c.writeContext(ctx, &Frame{ID: id, Op: OpPing}); err != nil {
		return nil, err
	}
	select {
	case f := <-ch:
		if f.Error != "" {
			return nil, errors.New(f.Error)
		}
		return &f, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type ExecResult struct {
	ExitCode int
}

func (c *Client) Exec(cmd []string, stdin io.Reader, stdout, stderr io.Writer, timeoutS int) (*ExecResult, error) {
	return c.ExecStream(cmd, stdin, timeoutS, func(f *Frame) {
		if f.Stream != StreamStdout && f.Stream != StreamStderr {
			return
		}
		data, derr := base64.StdEncoding.DecodeString(f.DataB64)
		if derr != nil {
			return
		}
		w := stdout
		if f.Stream == StreamStderr {
			w = stderr
		}
		if w != nil {
			w.Write(data)
		}
	})
}

// In-guest spawn failures are plain errors; transport failures wrap ErrUnreachable.
func (c *Client) ExecStream(cmd []string, stdin io.Reader, timeoutS int, onFrame func(*Frame)) (*ExecResult, error) {
	return c.ExecStreamContext(context.Background(), cmd, stdin, timeoutS, onFrame)
}

func (c *Client) ExecStreamContext(ctx context.Context, cmd []string, stdin io.Reader, timeoutS int, onFrame func(*Frame)) (*ExecResult, error) {
	if len(cmd) == 0 {
		return nil, fmt.Errorf("empty cmd")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	id := newID()
	ch := c.register(id)
	ack := c.register(id + "/stdin")
	defer c.unregister(id)
	defer c.unregister(id + "/stdin")
	if err := c.writeContext(ctx, &Frame{ID: id, Op: OpExec, Cmd: cmd, TimeoutS: timeoutS}); err != nil {
		return nil, err
	}
	completed := false
	defer func() {
		if !completed {
			go func() {
				if err := c.writeWithDeadline(&Frame{ID: id, Op: OpCancel}, time.Now().Add(time.Second)); err != nil {
					// If cancellation cannot be delivered, ending the session
					// is the only way to guarantee the guest cancels the exec.
					c.kill(err)
				}
			}()
		}
	}()
	go c.execStdin(ctx, id, stdin, ack)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case fr := <-ch:
			if fr.Error != "" {
				return nil, errors.New(fr.Error)
			}
			if onFrame != nil {
				onFrame(&fr)
			}
			if fr.Done {
				completed = true
				return &ExecResult{ExitCode: fr.ExitCode}, nil
			}
			if fr.Stream != "" {
				if err := c.writeContext(ctx, &Frame{ID: id, Op: OpOutputAck}); err != nil {
					return nil, err
				}
			}
		}
	}
}

func (c *Client) execStdin(ctx context.Context, id string, r io.Reader, ack <-chan Frame) {
	buf := make([]byte, 32<<10)
	for {
		if ctx.Err() != nil {
			return
		}
		n, err := 0, io.EOF
		if r != nil {
			n, err = r.Read(buf)
		}
		if n > 0 {
			if c.writeContext(ctx, &Frame{ID: id, Op: OpStdin, DataB64: base64.StdEncoding.EncodeToString(buf[:n])}) != nil {
				return
			}
			select {
			case f := <-ack:
				if f.Error != "" {
					return
				}
			case <-ctx.Done():
				return
			}
		}
		if err != nil {
			c.writeContext(ctx, &Frame{ID: id, Op: OpStdin, EOF: true})
			return
		}
	}
}

func (c *Client) unregister(id string) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *Client) register(id string) chan Frame {
	ch := make(chan Frame, 64)
	c.mu.Lock()
	if c.closed {
		ch <- Frame{ID: id, Error: ErrUnreachable.Error()}
	} else {
		c.pending[id] = ch
	}
	c.mu.Unlock()
	return ch
}

func (c *Client) Close() error {
	c.failAll(net.ErrClosed)
	return c.conn.Close()
}

func (c *Client) Closed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

var idCounter struct {
	sync.Mutex
	n int
}

func newID() string {
	idCounter.Lock()
	defer idCounter.Unlock()
	idCounter.n++
	return fmt.Sprintf("e%d-%d", time.Now().UnixNano(), idCounter.n)
}

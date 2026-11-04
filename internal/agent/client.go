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

type Client struct {
	sock string
	conn net.Conn
	w    *frameWriter
	wmu  sync.Mutex
	r    *bufio.Reader

	mu      sync.Mutex
	pending map[string]chan Frame
	control ControlHandler
	ctrlSem chan struct{}
	busySem chan struct{}
	closed  bool

	done     chan struct{} // closed on connection loss or Close
	doneOnce sync.Once
}

// DialSock connects to the agent channel at a unix socket path.
func DialSock(sock string) (*Client, error) {
	conn, err := net.DialTimeout("unix", sock, 5*time.Second)
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
			// Bounded admission BEFORE any goroutine spawns: the
			// semaphore caps running handlers, a smaller one caps
			// in-flight refusals, and past both, frames are dropped
			// (the guest's own request timeout is the backstop) — a
			// flooding guest cannot accumulate host goroutines.
			ctrlSem, busySem := c.controlSems()
			select {
			case ctrlSem <- struct{}{}:
				go c.dispatchControl(&f)
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
		ch := c.pending[f.ID]
		if f.Done || f.Error != "" {
			delete(c.pending, f.ID)
		}
		c.mu.Unlock()
		if ch != nil {
			ch <- f
			if f.Done || f.Error != "" {
				close(ch)
			}
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

// controlSems lazily creates the admission semaphores (tests construct
// Clients directly).
func (c *Client) controlSems() (chan struct{}, chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctrlSem == nil {
		c.ctrlSem = make(chan struct{}, maxConcurrentControls)
	}
	if c.busySem == nil {
		c.busySem = make(chan struct{}, maxConcurrentControlRefusals)
	}
	return c.ctrlSem, c.busySem
}

// dispatchControl runs an admitted control request; the semaphore slot
// is released when the response write completes or times out.
func (c *Client) dispatchControl(f *Frame) {
	defer func() { <-c.ctrlSem }()
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
	c.writeWithDeadline(&resp, time.Now().Add(controlWriteTimeout))
}

func (c *Client) writeControlBusy(f *Frame) {
	defer func() { <-c.busySem }()
	c.writeWithDeadline(&Frame{ID: f.ID, Error: "control busy"}, time.Now().Add(controlBusyWriteTimeout))
}

// runControl executes the handler under an overall deadline that is
// also cancelled when the connection drops, so abandoned requests stop
// consuming host resources.
func (c *Client) runControl(h ControlHandler, f *Frame) (string, json.RawMessage, string) {
	ctx, cancel := context.WithTimeout(context.Background(), controlHandlerTimeout)
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
	if c.done == nil {
		return
	}
	c.doneOnce.Do(func() { close(c.done) })
}

func (c *Client) failAll(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.signalDone()
	for id, ch := range c.pending {
		delete(c.pending, id)
		ch <- Frame{ID: id, Error: fmt.Sprintf("%v: %v", ErrUnreachable, err)}
		close(ch)
	}
}

func (c *Client) write(f *Frame) error {
	return c.writeWithDeadline(f, time.Time{})
}

// writeWithDeadline serializes the frame write and bounds how long it
// may stall on a peer that stopped reading; the deadline is cleared
// afterwards so stream writes are unaffected.
func (c *Client) writeWithDeadline(f *Frame, dl time.Time) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return ErrUnreachable
	}
	if !dl.IsZero() {
		c.conn.SetWriteDeadline(dl)
		defer c.conn.SetWriteDeadline(time.Time{})
	}
	if err := c.w.write(f); err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	return nil
}

func (c *Client) Ping() (*Frame, error) {
	id := newID()
	ch := c.register(id)
	if err := c.write(&Frame{ID: id, Op: OpPing}); err != nil {
		return nil, err
	}
	select {
	case f := <-ch:
		if f.Error != "" {
			return nil, errors.New(f.Error)
		}
		return &f, nil
	case <-time.After(10 * time.Second):
		return nil, fmt.Errorf("%w: ping timeout", ErrUnreachable)
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
	if len(cmd) == 0 {
		return nil, fmt.Errorf("empty cmd")
	}
	id := newID()
	ch := c.register(id)
	if err := c.write(&Frame{ID: id, Op: OpExec, Cmd: cmd, TimeoutS: timeoutS}); err != nil {
		return nil, err
	}
	if stdin != nil {
		go c.execStdin(id, stdin)
	}

	res := &ExecResult{ExitCode: -1}
	for f := range ch {
		fr := f
		switch {
		case fr.Error != "":
			return nil, errors.New(fr.Error)
		case fr.Done:
			res.ExitCode = fr.ExitCode
			if onFrame != nil {
				onFrame(&fr)
			}
			return res, nil
		default:
			if onFrame != nil {
				onFrame(&fr)
			}
		}
	}
	return nil, fmt.Errorf("%w: channel closed mid-exec", ErrUnreachable)
}

// execStdin streams r to an in-flight exec as stdin frames, then sends
// the EOF frame.
func (c *Client) execStdin(id string, r io.Reader) {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if werr := c.write(&Frame{ID: id, Op: OpStdin, DataB64: base64.StdEncoding.EncodeToString(buf[:n])}); werr != nil {
				return
			}
		}
		if err != nil {
			c.write(&Frame{ID: id, Op: OpStdin, EOF: true})
			return
		}
	}
}

func (c *Client) register(id string) chan Frame {
	ch := make(chan Frame, 64)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	return ch
}

func (c *Client) Close() error {
	c.signalDone()
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

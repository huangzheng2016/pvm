package agent

import (
	"bufio"
	"encoding/base64"
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

// Client is the host side of the channel, dialing the unix socket QEMU
// exposes for the VM's virtio-serial port.
type Client struct {
	sock string
	conn net.Conn
	w    *frameWriter
	wmu  sync.Mutex
	r    *bufio.Reader

	mu      sync.Mutex
	pending map[string]chan Frame
	closed  bool
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

func (c *Client) failAll(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for id, ch := range c.pending {
		delete(c.pending, id)
		ch <- Frame{ID: id, Error: fmt.Sprintf("%v: %v", ErrUnreachable, err)}
		close(ch)
	}
}

func (c *Client) write(f *Frame) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return ErrUnreachable
	}
	if err := c.w.write(f); err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	return nil
}

// Ping checks agent liveness, returning its version and hostname.
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

// ExecResult collects a completed exec.
type ExecResult struct {
	ExitCode int
}

// Exec runs cmd in the guest, streaming output to stdout/stderr
// (nil sinks discard). stdin may be nil.
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

// ExecStream runs cmd in the guest, invoking onFrame for every frame
// (output, done) as it arrives; stdin may be nil. The command's exit
// code is in the result; in-guest spawn failures come back as plain
// errors, transport failures as ErrUnreachable.
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

func (c *Client) Close() error { return c.conn.Close() }

// Closed reports whether the read loop has ended (peer gone or error).
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

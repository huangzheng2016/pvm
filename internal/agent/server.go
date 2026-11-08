package agent

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/huangzheng2016/pvm/internal/buildinfo"
)

const maxConcurrentExecs = 128

type execSession struct {
	stdin     chan Frame
	outputAck chan struct{}
	cancel    context.CancelFunc
}

type Server struct {
	Version string

	started time.Time
	mu      sync.Mutex // guards execs
	execs   map[string]*execSession

	ctrlMu       sync.Mutex // guards ctrlPending and the session write
	ctrlPending  map[string]chan Frame
	sessionWrite func(*Frame) error
	sessionSeq   uint64
}

func NewServer() *Server {
	return &Server{
		Version:     buildinfo.Version,
		started:     time.Now(),
		execs:       map[string]*execSession{},
		ctrlPending: map[string]chan Frame{},
	}
}

// Run opens the virtio-serial port and serves forever.
//
// The device fd is opened ONCE and never closed: the linux
// virtio_console driver answers reads with an immediate EOF while the
// host side is disconnected, and close+reopen does not reliably restore
// the port. qemu-guest-agent uses the same model — keep the fd, treat
// EOF as "host away", back off, and keep reading on the same fd; reads
// start blocking for data again once the host reconnects.
func (s *Server) Run(sysfsRoot string) error {
	dev, err := FindVirtioPort(sysfsRoot, PortName)
	if err != nil {
		return err
	}
	log.Printf("agent: serving on %s (version %s)", dev, s.Version)
	go func() {
		if err := s.ServeControl(ControlSock); err != nil {
			log.Printf("agent: control broker stopped: %v", err)
		}
	}()
	var f *os.File
	for {
		if f == nil {
			f, err = os.OpenFile(dev, os.O_RDWR, 0)
			if err != nil {
				log.Printf("agent: open %s: %v (retry in 2s)", dev, err)
				time.Sleep(2 * time.Second)
				continue
			}
		}
		start := time.Now()
		err := s.Serve(f)
		// Instant EOFs mean "no host connected" — expected while idle,
		// so only log sessions that actually carried traffic.
		if err != nil && time.Since(start) > 2*time.Second {
			log.Printf("agent: host disconnected: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// Serve leaves rw open for the next host session and kills all session
// execs on return.
func (s *Server) Serve(rw io.ReadWriteCloser) error {
	serveCtx, cancelExecs := context.WithCancel(context.Background())

	w := newFrameWriter(rw)
	var wmu sync.Mutex
	write := func(f *Frame) error {
		wmu.Lock()
		defer wmu.Unlock()
		return w.write(f)
	}

	s.ctrlMu.Lock()
	s.sessionSeq++
	seq := s.sessionSeq
	s.sessionWrite = write
	s.ctrlMu.Unlock()
	var wg sync.WaitGroup
	defer func() {
		s.ctrlMu.Lock()
		if s.sessionSeq == seq {
			s.sessionWrite = nil
		}
		s.failCtrlPendingLocked("host session ended")
		s.ctrlMu.Unlock()
		cancelExecs()
		wg.Wait()
	}()

	r := bufio.NewReaderSize(rw, 64<<10)
	for {
		var f Frame
		if err := readFrame(r, &f); err != nil {
			return err
		}
		if f.Op == OpResult || (f.Op == "" && f.Error != "") {
			if s.deliverControl(&f) {
				continue
			}
		}
		switch f.Op {
		case OpPing:
			host, _ := os.Hostname()
			write(&Frame{
				ID: f.ID, Op: OpPong, Version: s.Version,
				Hostname: host, UptimeS: int64(time.Since(s.started).Seconds()),
			})
		case OpExec:
			ctx, cancel := context.WithCancel(serveCtx)
			session := &execSession{stdin: make(chan Frame, 2), outputAck: make(chan struct{}, 2), cancel: cancel}
			s.mu.Lock()
			_, duplicate := s.execs[f.ID]
			admitted := !duplicate && len(s.execs) < maxConcurrentExecs
			if admitted {
				s.execs[f.ID] = session
			}
			s.mu.Unlock()
			if !admitted {
				cancel()
				if duplicate {
					return fmt.Errorf("duplicate exec id %s", f.ID)
				}
				write(&Frame{ID: f.ID, Error: "exec capacity exceeded"})
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer cancel()
				defer s.untrack(f.ID)
				s.runExec(ctx, &f, session, write)
			}()
		case OpStdin, OpOutputAck, OpCancel:
			s.mu.Lock()
			session := s.execs[f.ID]
			s.mu.Unlock()
			if session == nil {
				continue
			}
			switch f.Op {
			case OpCancel:
				session.cancel()
			case OpOutputAck:
				select {
				case session.outputAck <- struct{}{}:
				default:
				}
			case OpStdin:
				select {
				case session.stdin <- f:
				default:
					session.cancel()
					write(&Frame{ID: f.ID, Error: "stdin flow-control window exceeded"})
				}
			}
		default:
			write(&Frame{ID: f.ID, Error: "unknown op " + f.Op})
		}
	}
}

func (s *Server) untrack(id string) {
	s.mu.Lock()
	delete(s.execs, id)
	s.mu.Unlock()
}

// deliverControl routes a result/error frame to its pending control
// request; it reports whether the id belonged to one.
func (s *Server) deliverControl(f *Frame) bool {
	s.ctrlMu.Lock()
	ch, ok := s.ctrlPending[f.ID]
	if ok {
		delete(s.ctrlPending, f.ID)
	}
	s.ctrlMu.Unlock()
	if !ok {
		return false
	}
	// The channel has capacity one and only one terminal frame is ever
	// delivered, so this cannot block.
	ch <- *f
	return true
}

// failCtrlPendingLocked errors every in-flight control request so
// callers never hang on a dead session. Callers hold ctrlMu.
func (s *Server) failCtrlPendingLocked(msg string) {
	for id, ch := range s.ctrlPending {
		delete(s.ctrlPending, id)
		ch <- Frame{ID: id, Error: msg}
	}
}

// Control forwards one scoped control request to the host over the
// active session and waits for its result. It fails fast while no host
// session is attached.
func (s *Server) Control(action string, payload []byte, timeout time.Duration) ([]byte, error) {
	if len(payload) > MaxControlPayloadBytes {
		return nil, fmt.Errorf("control payload exceeds %d bytes", MaxControlPayloadBytes)
	}
	id := newControlID()
	ch := make(chan Frame, 1)
	s.ctrlMu.Lock()
	if s.ctrlPending == nil {
		s.ctrlPending = map[string]chan Frame{}
	}
	s.ctrlPending[id] = ch
	write := s.sessionWrite
	s.ctrlMu.Unlock()
	if write == nil {
		s.untrackControl(id)
		return nil, fmt.Errorf("%w: host not connected", ErrUnreachable)
	}
	if err := write(&Frame{ID: id, Op: OpControl, Action: action, Payload: payload}); err != nil {
		s.untrackControl(id)
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	select {
	case f := <-ch:
		if f.Error != "" {
			return nil, errors.New(f.Error)
		}
		return f.Payload, nil
	case <-time.After(timeout):
		s.untrackControl(id)
		return nil, fmt.Errorf("control %s: no result within %s", action, timeout)
	}
}

func (s *Server) untrackControl(id string) {
	s.ctrlMu.Lock()
	delete(s.ctrlPending, id)
	s.ctrlMu.Unlock()
}

func (s *Server) runExec(serveCtx context.Context, f *Frame, session *execSession, write func(*Frame) error) {
	if len(f.Cmd) == 0 {
		write(&Frame{ID: f.ID, Error: "exec with empty cmd"})
		return
	}
	ctx := serveCtx
	if f.TimeoutS > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(serveCtx, time.Duration(f.TimeoutS)*time.Second)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, f.Cmd[0], f.Cmd[1:]...)
	// own process group so a timeout kills the whole tree
	configureExecProcessGroup(cmd)

	stdinW, err := cmd.StdinPipe()
	if err != nil {
		write(&Frame{ID: f.ID, Error: err.Error()})
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		write(&Frame{ID: f.ID, Error: err.Error()})
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		write(&Frame{ID: f.ID, Error: err.Error()})
		return
	}
	cmd.Cancel = func() error {
		err := killExecProcessGroup(cmd)
		// Descendants can retain these pipes while cancellation races with fork.
		// Closing our ends lets Wait reap the leader without waiting for EOF.
		stdinW.Close()
		stdout.Close()
		stderr.Close()
		return err
	}
	if err := cmd.Start(); err != nil {
		write(&Frame{ID: f.ID, Error: err.Error()})
		return
	}

	// stdin forwarder: frames -> the command's stdin pipe. Using
	// StdinPipe (not cmd.Stdin = reader) because Wait would otherwise
	// block on the internal copy goroutine after the process exits.
	stop := make(chan struct{})
	var inWg sync.WaitGroup
	inWg.Add(1)
	go func() {
		defer inWg.Done()
		for {
			select {
			case f, ok := <-session.stdin:
				if !ok {
					return
				}
				if f.EOF {
					stdinW.Close()
					write(&Frame{ID: f.ID, Op: OpStdinAck})
					return
				}
				data, err := base64.StdEncoding.DecodeString(f.DataB64)
				if err != nil {
					session.cancel()
					write(&Frame{ID: f.ID, Error: "invalid stdin encoding"})
					return
				}
				if _, err := stdinW.Write(data); err != nil {
					return // process gone; Wait closes the pipe
				}
				if err := write(&Frame{ID: f.ID, Op: OpStdinAck}); err != nil {
					session.cancel()
					return
				}
			case <-stop:
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	var cp sync.WaitGroup
	cp.Add(2)
	for _, p := range []struct {
		stream string
		src    io.ReadCloser
	}{{StreamStdout, stdout}, {StreamStderr, stderr}} {
		go func(stream string, src io.ReadCloser) {
			defer cp.Done()
			buf := make([]byte, 32<<10)
			for {
				n, err := src.Read(buf)
				if n > 0 {
					if err := write(&Frame{ID: f.ID, Stream: stream, DataB64: base64.StdEncoding.EncodeToString(buf[:n])}); err != nil {
						session.cancel()
						return
					}
					select {
					case <-session.outputAck:
					case <-ctx.Done():
						return
					}
				}
				if err != nil {
					return
				}
			}
		}(p.stream, p.src)
	}

	// Drain stdout/stderr BEFORE cmd.Wait: Wait closes the pipes on
	// return and would truncate in-flight reads (os/exec docs:
	// "it is incorrect to call Wait before all reads from the pipe
	// have completed"). The pipes hit EOF when the process exits, so
	// this ordering cannot deadlock — the ctx kill still fires first
	// for timeouts.
	cp.Wait()
	err = cmd.Wait()
	if ctx.Err() != nil {
		// A child created concurrently with the first group signal may survive
		// it. The reaped leader can no longer create additional children.
		_ = killExecProcessGroup(cmd)
	}
	close(stop)
	stdinW.Close()
	inWg.Wait()

	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			write(&Frame{ID: f.ID, Error: err.Error()})
			return
		}
	}
	write(&Frame{ID: f.ID, Done: true, ExitCode: code})
}

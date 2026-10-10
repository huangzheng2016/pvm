package agent

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/huangzheng2016/pvm/internal/buildinfo"
)

// Server is the guest-side agent: one host connection, many concurrent
// exec sessions keyed by frame id.
type Server struct {
	Version string

	started time.Time
	mu      sync.Mutex // guards execs
	execs   map[string]chan Frame
}

func NewServer() *Server {
	return &Server{Version: buildinfo.Version, started: time.Now(), execs: map[string]chan Frame{}}
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

// Serve handles one host session on rw until EOF/hangup or a fatal
// write error. rw stays open — the caller reuses it for the next
// session. All execs spawned during the session are killed on return.
// Exported for tests, which can drive it over net.Pipe.
func (s *Server) Serve(rw io.ReadWriteCloser) error {
	serveCtx, cancelExecs := context.WithCancel(context.Background())
	defer cancelExecs()

	w := newFrameWriter(rw)
	var wmu sync.Mutex
	write := func(f *Frame) error {
		wmu.Lock()
		defer wmu.Unlock()
		return w.write(f)
	}

	r := bufio.NewReaderSize(rw, 64<<10)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		var f Frame
		if err := readFrame(r, &f); err != nil {
			return err
		}
		switch f.Op {
		case OpPing:
			host, _ := os.Hostname()
			write(&Frame{
				ID: f.ID, Op: OpPong, Version: s.Version,
				Hostname: host, UptimeS: int64(time.Since(s.started).Seconds()),
			})
		case OpExec:
			// Register the stdin channel before spawning so early
			// stdin frames are queued, not dropped.
			ch := s.track(f.ID)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer s.untrack(f.ID)
				s.runExec(serveCtx, &f, ch, write)
			}()
		case OpStdin:
			if ch := s.stdinOf(f.ID); ch != nil {
				select {
				case ch <- f:
				default: // exec gone or flooded; drop
				}
			}
		default:
			write(&Frame{ID: f.ID, Error: "unknown op " + f.Op})
		}
	}
}

func (s *Server) track(id string) chan Frame {
	ch := make(chan Frame, 64)
	s.mu.Lock()
	s.execs[id] = ch
	s.mu.Unlock()
	return ch
}

func (s *Server) untrack(id string) {
	s.mu.Lock()
	if ch, ok := s.execs[id]; ok {
		delete(s.execs, id)
		close(ch)
	}
	s.mu.Unlock()
}

func (s *Server) stdinOf(id string) chan Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.execs[id]; ok {
		return ch
	}
	return nil
}

func (s *Server) runExec(serveCtx context.Context, f *Frame, stdinCh <-chan Frame, write func(*Frame) error) {
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
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

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
			case f, ok := <-stdinCh:
				if !ok {
					return
				}
				if f.EOF {
					stdinW.Close()
					return
				}
				data, err := base64.StdEncoding.DecodeString(f.DataB64)
				if err != nil {
					continue
				}
				if _, err := stdinW.Write(data); err != nil {
					return // process gone; Wait closes the pipe
				}
			case <-stop:
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
					write(&Frame{ID: f.ID, Stream: stream, DataB64: base64.StdEncoding.EncodeToString(buf[:n])})
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
	close(stop) // untrack (which closes stdinCh) runs only after we return
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

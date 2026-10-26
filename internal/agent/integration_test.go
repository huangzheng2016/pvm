package agent

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func pipePair(t *testing.T) (*Server, *Client) {
	t.Helper()
	h, g := net.Pipe()
	srv := NewServer()
	go srv.Serve(g)
	c := &Client{
		conn:    h,
		w:       newFrameWriter(h),
		r:       bufioReader(h),
		pending: map[string]chan Frame{},
	}
	go c.readLoop()
	t.Cleanup(func() { c.Close() })
	return srv, c
}

func bufioReader(r net.Conn) *bufio.Reader {
	return bufio.NewReaderSize(r, 64<<10)
}

func TestPing(t *testing.T) {
	_, c := pipePair(t)
	f, err := c.Ping()
	if err != nil {
		t.Fatal(err)
	}
	if f.Op != OpPong || f.Version == "" {
		t.Fatalf("bad pong: %+v", f)
	}
}

func TestExecEcho(t *testing.T) {
	_, c := pipePair(t)
	var out, errb bytes.Buffer
	res, err := c.Exec([]string{"echo", "ok"}, nil, &out, &errb, 10)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit %d", res.ExitCode)
	}
	if strings.TrimSpace(out.String()) != "ok" {
		t.Fatalf("stdout %q", out.String())
	}
}

func TestExecExitCodeAndStderr(t *testing.T) {
	_, c := pipePair(t)
	var out, errb bytes.Buffer
	res, err := c.Exec([]string{"sh", "-c", "echo errline >&2; exit 7"}, nil, &out, &errb, 10)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 7 {
		t.Fatalf("exit %d, want 7", res.ExitCode)
	}
	if !strings.Contains(errb.String(), "errline") {
		t.Fatalf("stderr %q", errb.String())
	}
}

func TestExecStdin(t *testing.T) {
	_, c := pipePair(t)
	var out bytes.Buffer
	res, err := c.Exec([]string{"cat"}, strings.NewReader("piped-in"), &out, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || out.String() != "piped-in" {
		t.Fatalf("exit %d out %q", res.ExitCode, out.String())
	}
}

func TestExecTimeout(t *testing.T) {
	_, c := pipePair(t)
	start := time.Now()
	res, err := c.Exec([]string{"sleep", "30"}, nil, nil, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 {
		t.Fatal("killed process must not exit 0")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("timeout not enforced, took %v", time.Since(start))
	}
}

func TestExecConcurrent(t *testing.T) {
	_, c := pipePair(t)
	const n = 10
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var out bytes.Buffer
			want := fmt.Sprintf("worker-%d", i)
			res, err := c.Exec([]string{"sh", "-c", "sleep 0.1; echo " + want}, nil, &out, nil, 30)
			if err != nil {
				errs <- fmt.Errorf("w%d: %w", i, err)
				return
			}
			if res.ExitCode != 0 {
				errs <- fmt.Errorf("w%d: exit %d", i, res.ExitCode)
				return
			}
			if strings.TrimSpace(out.String()) != want {
				errs <- fmt.Errorf("w%d: got %q want %q (cross-talk?)", i, strings.TrimSpace(out.String()), want)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestExecBadCommand(t *testing.T) {
	_, c := pipePair(t)
	_, err := c.Exec([]string{"/nonexistent-bin"}, nil, nil, nil, 5)
	if err == nil {
		t.Fatal("want spawn error")
	}
}

func TestDisconnectCancelsRunningExec(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	srv := NewServer()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(guest) }()
	if err := host.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := newFrameWriter(host).write(&Frame{
		ID: "disconnect", Op: OpExec,
		Cmd: []string{"sh", "-c", "echo ready; exec sleep 30"}, TimeoutS: 5,
	}); err != nil {
		t.Fatal(err)
	}
	var f Frame
	if err := readFrame(bufioReader(host), &f); err != nil {
		t.Fatal(err)
	}
	if f.Stream != StreamStdout {
		t.Fatalf("command did not start: %+v", f)
	}
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		// The command timeout still bounds cleanup if cancellation regresses.
		<-done
		t.Fatal("Serve did not cancel running exec after disconnect")
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.execs) != 0 {
		t.Fatalf("session retained %d execs", len(srv.execs))
	}
}

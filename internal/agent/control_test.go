package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func controlPair(t *testing.T, h ControlHandler) (*Server, *Client) {
	t.Helper()
	srv, c := pipePair(t)
	c.HandleControl(h)
	waitSession(t, srv)
	return srv, c
}

func TestControlRoundTrip(t *testing.T) {
	srv, _ := controlPair(t, func(ctx context.Context, req *Frame) ([]byte, error) {
		if req.Action != ControlMapList {
			t.Errorf("action = %q", req.Action)
		}
		return json.Marshal(MapListResponse{Maps: []MapEntry{{HostPort: 8080, GuestPort: 80, Proto: "tcp"}}})
	})
	payload, err := srv.Control(ControlMapList, nil, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var resp MapListResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Maps) != 1 || resp.Maps[0].HostPort != 8080 || resp.Maps[0].GuestPort != 80 {
		t.Fatalf("bad payload %s", payload)
	}
}

func TestControlHandlerError(t *testing.T) {
	srv, _ := controlPair(t, func(ctx context.Context, req *Frame) ([]byte, error) {
		return nil, fmt.Errorf("unknown control action %q", req.Action)
	})
	waitSession(t, srv)
	_, err := srv.Control("map.exfiltrate", nil, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "unknown control action") {
		t.Fatalf("want handler error, got %v", err)
	}
}

func TestControlNoSession(t *testing.T) {
	srv := NewServer()
	start := time.Now()
	_, err := srv.Control(ControlMapList, nil, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "host not connected") {
		t.Fatalf("want fast no-session error, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("no-session control must fail fast, not wait for the timeout")
	}
}

func TestControlSessionDropFailsPending(t *testing.T) {
	host, guest := net.Pipe()
	srv := NewServer()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(guest) }()
	c := &Client{conn: host, w: newFrameWriter(host), r: bufioReader(host), pending: map[string]chan Frame{}, done: make(chan struct{})}
	started := make(chan struct{}, 1)
	c.HandleControl(func(ctx context.Context, req *Frame) ([]byte, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	go c.readLoop()
	waitSession(t, srv)

	errCh := make(chan error, 1)
	go func() {
		_, err := srv.Control(ControlMapList, nil, time.Minute)
		errCh <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not start")
	}
	host.Close()
	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "host session ended") {
			t.Fatalf("want session-drop error, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pending control was not failed on disconnect")
	}
	<-done
	waitControlWorkers(t, c)
}

func TestControlReconnect(t *testing.T) {
	srv := NewServer()
	handler := func(ctx context.Context, req *Frame) ([]byte, error) {
		return json.Marshal(MapListResponse{Maps: []MapEntry{}})
	}

	host1, guest1 := net.Pipe()
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(guest1) }()
	c1 := &Client{conn: host1, w: newFrameWriter(host1), r: bufioReader(host1), pending: map[string]chan Frame{}}
	c1.HandleControl(handler)
	go c1.readLoop()
	waitSession(t, srv)
	if _, err := srv.Control(ControlMapList, nil, 5*time.Second); err != nil {
		t.Fatalf("session 1: %v", err)
	}
	host1.Close()
	<-serveDone

	// Between sessions the request fails fast instead of hanging.
	if _, err := srv.Control(ControlMapList, nil, 5*time.Second); err == nil ||
		!strings.Contains(err.Error(), "host not connected") {
		t.Fatalf("gap control = %v, want host not connected", err)
	}

	host2, guest2 := net.Pipe()
	serveDone2 := make(chan error, 1)
	go func() { serveDone2 <- srv.Serve(guest2) }()
	c2 := &Client{conn: host2, w: newFrameWriter(host2), r: bufioReader(host2), pending: map[string]chan Frame{}}
	c2.HandleControl(handler)
	go c2.readLoop()
	waitSession(t, srv)
	if _, err := srv.Control(ControlMapList, nil, 5*time.Second); err != nil {
		t.Fatalf("session 2: %v", err)
	}
	host2.Close()
	<-serveDone2
}

func waitSession(t *testing.T, srv *Server) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		srv.ctrlMu.Lock()
		active := srv.sessionWrite != nil
		srv.ctrlMu.Unlock()
		if active {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no active session")
}

func TestControlConcurrentExec(t *testing.T) {
	srv, c := controlPair(t, func(ctx context.Context, req *Frame) ([]byte, error) {
		return json.Marshal(MapListResponse{Maps: []MapEntry{}})
	})
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, 2*n)
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			var out bytes.Buffer
			want := fmt.Sprintf("worker-%d", i)
			res, err := c.Exec([]string{"sh", "-c", "sleep 0.05; echo " + want}, nil, &out, nil, 30)
			if err != nil {
				errs <- fmt.Errorf("exec %d: %w", i, err)
				return
			}
			if res.ExitCode != 0 || strings.TrimSpace(out.String()) != want {
				errs <- fmt.Errorf("exec %d: exit %d out %q (cross-talk?)", i, res.ExitCode, out.String())
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			payload, err := srv.Control(ControlMapList, nil, 10*time.Second)
			if err != nil {
				errs <- fmt.Errorf("control %d: %w", i, err)
				return
			}
			if !json.Valid(payload) {
				errs <- fmt.Errorf("control %d: invalid payload %q", i, payload)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestControlConcurrentControls(t *testing.T) {
	srv, _ := controlPair(t, func(ctx context.Context, req *Frame) ([]byte, error) {
		var in MapAddRequest
		if err := json.Unmarshal(req.Payload, &in); err != nil {
			return nil, err
		}
		return json.Marshal(MapAddResponse{Map: MapEntry{HostPort: in.HostPort, GuestPort: in.GuestPort, Proto: "tcp"}})
	})
	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, _ := json.Marshal(MapAddRequest{HostPort: 10000 + i, GuestPort: i})
			payload, err := srv.Control(ControlMapAdd, req, 10*time.Second)
			if err != nil {
				errs <- err
				return
			}
			var resp MapAddResponse
			if err := json.Unmarshal(payload, &resp); err != nil {
				errs <- err
				return
			}
			if resp.Map.HostPort != 10000+i || resp.Map.GuestPort != i {
				errs <- fmt.Errorf("response mismatch: %+v (cross-talk?)", resp.Map)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestControlTimeout(t *testing.T) {
	srv, _ := controlPair(t, func(ctx context.Context, req *Frame) ([]byte, error) {
		time.Sleep(300 * time.Millisecond)
		return []byte(`{}`), nil
	})
	start := time.Now()
	_, err := srv.Control(ControlMapList, nil, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "no result within") {
		t.Fatalf("want timeout, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("timeout not enforced")
	}
	// The late result must not poison the next request.
	payload, err := srv.Control(ControlMapList, nil, 5*time.Second)
	if err != nil || !json.Valid(payload) {
		t.Fatalf("post-timeout control: %v %q", err, payload)
	}
}

func TestControlOversizedPayload(t *testing.T) {
	srv, _ := controlPair(t, func(ctx context.Context, req *Frame) ([]byte, error) {
		return []byte(`{}`), nil
	})
	big := bytes.Repeat([]byte("x"), MaxControlPayloadBytes+1)
	if _, err := srv.Control(ControlMapAdd, big, 5*time.Second); err == nil ||
		!strings.Contains(err.Error(), "payload exceeds") {
		t.Fatalf("local oversized send = %v", err)
	}
}

func TestControlOversizedRequestRejected(t *testing.T) {
	// An oversized control request gets an error result, not a handler
	// call. The frame arrives from the guest end of the channel.
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	called := make(chan struct{}, 1)
	c := &Client{conn: host, w: newFrameWriter(host), r: bufioReader(host), pending: map[string]chan Frame{}}
	c.HandleControl(func(ctx context.Context, req *Frame) ([]byte, error) {
		called <- struct{}{}
		return []byte(`{}`), nil
	})
	go c.readLoop()
	// Payload must itself be valid JSON; a quoted string keeps it raw on
	// the wire while pushing the decoded size past the cap.
	oversized := json.RawMessage(`"` + strings.Repeat("x", MaxControlPayloadBytes) + `"`)
	go newFrameWriter(guest).write(&Frame{ID: "x", Op: OpControl, Action: ControlMapAdd, Payload: oversized})
	var f Frame
	if err := readFrame(bufio.NewReaderSize(guest, 64<<10), &f); err != nil {
		t.Fatal(err)
	}
	if f.ID != "x" || !strings.Contains(f.Error, "too large") {
		t.Fatalf("want payload-too-large error, got %+v", f)
	}
	select {
	case <-called:
		t.Fatal("handler must not be called for an oversized payload")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestControlResultTooLarge(t *testing.T) {
	srv, _ := controlPair(t, func(ctx context.Context, req *Frame) ([]byte, error) {
		return bytes.Repeat([]byte("x"), MaxControlPayloadBytes+1), nil
	})
	_, err := srv.Control(ControlMapList, nil, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "result too large") {
		t.Fatalf("want result-too-large error, got %v", err)
	}
}

func TestServeMalformedFrame(t *testing.T) {
	host, guest := net.Pipe()
	srv := NewServer()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(guest) }()
	if _, err := host.Write([]byte("not json\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "bad frame") {
			t.Fatalf("want bad frame error, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not reject a malformed frame")
	}
}

func TestServeUnknownOpStillAnswered(t *testing.T) {
	srv := NewServer()
	host, guest := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(guest) }()
	defer host.Close()
	if err := newFrameWriter(host).write(&Frame{ID: "u1", Op: "make-coffee"}); err != nil {
		t.Fatal(err)
	}
	var f Frame
	if err := readFrame(bufio.NewReaderSize(host, 64<<10), &f); err != nil {
		t.Fatal(err)
	}
	if f.ID != "u1" || !strings.Contains(f.Error, "unknown op") {
		t.Fatalf("want unknown-op error, got %+v", f)
	}
}

func TestControlCallBrokerRoundTrip(t *testing.T) {
	srv, _ := controlPair(t, func(ctx context.Context, req *Frame) ([]byte, error) {
		var in MapAddRequest
		if err := json.Unmarshal(req.Payload, &in); err != nil {
			return nil, err
		}
		return json.Marshal(MapAddResponse{Map: MapEntry{HostPort: in.HostPort, GuestPort: in.GuestPort, Proto: "tcp"}})
	})
	sock := brokerSock(t)
	go srv.ServeControl(sock)
	waitBroker(t, sock)

	var resp MapAddResponse
	if err := ControlCall(sock, ControlMapAdd, &MapAddRequest{HostPort: 18080, GuestPort: 80}, &resp, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if resp.Map.HostPort != 18080 || resp.Map.GuestPort != 80 {
		t.Fatalf("bad response %+v", resp)
	}
}

func TestControlCallBrokerErrorPropagates(t *testing.T) {
	srv, _ := controlPair(t, func(ctx context.Context, req *Frame) ([]byte, error) {
		return nil, errors.New("port 80/tcp is not mapped by this vm")
	})
	sock := brokerSock(t)
	go srv.ServeControl(sock)
	waitBroker(t, sock)
	err := ControlCall(sock, ControlMapRemove, &MapRemoveRequest{HostPort: 80}, nil, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "not mapped by this vm") {
		t.Fatalf("want ownership error, got %v", err)
	}
}

func TestControlCallBrokerNoHostSession(t *testing.T) {
	srv := NewServer() // no Serve: host away
	sock := brokerSock(t)
	go srv.ServeControl(sock)
	waitBroker(t, sock)
	err := ControlCall(sock, ControlMapList, nil, nil, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "host not connected") {
		t.Fatalf("want host-not-connected, got %v", err)
	}
}

func TestControlCallBrokerMalformed(t *testing.T) {
	srv := NewServer()
	sock := brokerSock(t)
	go srv.ServeControl(sock)
	waitBroker(t, sock)

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("garbage\n")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("broker must drop malformed local requests")
	}
	conn.Close()
}

func TestControlCallBrokerRejectsNonControl(t *testing.T) {
	srv := NewServer()
	sock := brokerSock(t)
	go srv.ServeControl(sock)
	waitBroker(t, sock)

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := newFrameWriter(conn).write(&Frame{ID: "p1", Op: OpPing}); err != nil {
		t.Fatal(err)
	}
	var f Frame
	if err := readFrame(bufio.NewReaderSize(conn, 64<<10), &f); err != nil {
		t.Fatal(err)
	}
	if f.Error == "" || !strings.Contains(f.Error, "not a control request") {
		t.Fatalf("want not-a-control-request, got %+v", f)
	}
}

func TestControlCallBrokerDown(t *testing.T) {
	err := ControlCall(brokerSock(t), ControlMapList, nil, nil, time.Second)
	if err == nil || !strings.Contains(err.Error(), "agent broker") {
		t.Fatalf("want broker dial error, got %v", err)
	}
}

// brokerSock returns a short unique unix socket path: macOS sun_path
// holds 104 bytes, which t.TempDir()-based paths can exceed.
func brokerSock(t *testing.T) string {
	t.Helper()
	sock := filepath.Join(os.TempDir(), fmt.Sprintf("pvm-agent-test-%d-%d.sock", os.Getpid(), brokerSockSeq.Add(1)))
	t.Cleanup(func() { os.Remove(sock) })
	return sock
}

var brokerSockSeq atomic.Int64

func waitBroker(t *testing.T, sock string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", sock)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("broker did not start")
}

func TestControlConcurrencyLimit(t *testing.T) {
	accepted := maxConcurrentControls + maxQueuedControls
	const extra = 3
	total := accepted + extra
	entered := make(chan struct{}, total)
	release := make(chan struct{})
	srv, _ := controlPair(t, func(ctx context.Context, req *Frame) ([]byte, error) {
		entered <- struct{}{}
		<-release
		return []byte(`{}`), nil
	})

	// Eight handlers run and another maxQueuedControls wait in the queue.
	// Only requests beyond that fixed capacity may be refused or dropped.
	var wg sync.WaitGroup
	errs := make(chan error, total)
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := srv.Control(ControlMapList, nil, 2*time.Second)
			errs <- err
		}()
	}
	// While the first maxConcurrentControls handlers are blocked, the
	// overflow refusals must come back without queueing.
	busy := 0
	for busy < maxConcurrentControlRefusals {
		select {
		case err := <-errs:
			if err != nil && strings.Contains(err.Error(), "control busy") {
				busy++
			} else {
				t.Fatalf("expected control busy, got %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d/%d refusals arrived", busy, maxConcurrentControlRefusals)
		}
	}
	close(release)
	wg.Wait()
	close(errs)
	done, dropped := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			done++
		case strings.Contains(err.Error(), "no result within"):
			dropped++
		case strings.Contains(err.Error(), "control busy"):
			busy++
		}
	}
	if done != accepted {
		t.Fatalf("done = %d, want %d", done, accepted)
	}
	if busy+dropped != extra {
		t.Fatalf("busy = %d, dropped = %d; want %d excess accounted", busy, dropped, extra)
	}
}

func TestControlHandlerDeadline(t *testing.T) {
	srv, c := controlPair(t, func(ctx context.Context, req *Frame) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	c.ctrlHandlerTimeout = 100 * time.Millisecond
	start := time.Now()
	_, err := srv.Control(ControlMapList, nil, 20*time.Second)
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("want deadline error, got %v", err)
	}
	if d := time.Since(start); d < 100*time.Millisecond || d > 10*time.Second {
		t.Fatalf("deadline not enforced, took %v", d)
	}
}

func TestControlDisconnectCancelsHandler(t *testing.T) {
	host, guest := net.Pipe()
	srv := NewServer()
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(guest) }()
	handlerStarted := make(chan struct{}, 1)
	handlerDone := make(chan error, 1)
	c := &Client{
		conn:    host,
		w:       newFrameWriter(host),
		r:       bufioReader(host),
		pending: map[string]chan Frame{},
		done:    make(chan struct{}),
	}
	c.HandleControl(func(ctx context.Context, req *Frame) ([]byte, error) {
		handlerStarted <- struct{}{}
		<-ctx.Done() // cancelled by the disconnect, not by the deadline
		handlerDone <- ctx.Err()
		return nil, ctx.Err()
	})
	go c.readLoop()
	waitSession(t, srv)

	errCh := make(chan error, 1)
	go func() {
		_, err := srv.Control(ControlMapList, nil, time.Minute)
		errCh <- err
	}()
	select {
	case <-handlerStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not start")
	}
	c.Close()
	select {
	case err := <-handlerDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("handler ctx err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler was not cancelled on disconnect")
	}
	<-errCh
	<-serveDone
	waitControlWorkers(t, c)
}

func TestControlDisconnectFailsPendingAndStopsWorkers(t *testing.T) {
	host, guest := net.Pipe()
	srv := NewServer()
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(guest) }()
	c := &Client{
		conn:    host,
		w:       newFrameWriter(host),
		r:       bufioReader(host),
		pending: map[string]chan Frame{},
		done:    make(chan struct{}),
	}
	defer c.Close()
	total := maxConcurrentControls + maxQueuedControls
	started := make(chan struct{}, total)
	var calls atomic.Int32
	c.HandleControl(func(ctx context.Context, req *Frame) ([]byte, error) {
		calls.Add(1)
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	go c.readLoop()
	waitSession(t, srv)

	errs := make(chan error, total)
	for i := 0; i < maxConcurrentControls; i++ {
		go func() {
			_, err := srv.Control(ControlMapList, nil, time.Minute)
			errs <- err
		}()
	}
	for i := 0; i < maxConcurrentControls; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d handlers started", i)
		}
	}
	for i := maxConcurrentControls; i < total; i++ {
		go func() {
			_, err := srv.Control(ControlMapList, nil, time.Minute)
			errs <- err
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(c.ctrlQueue) != maxQueuedControls && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := len(c.ctrlQueue); got != maxQueuedControls {
		t.Fatalf("queued = %d, want %d", got, maxQueuedControls)
	}

	c.Close()
	for i := 0; i < total; i++ {
		select {
		case err := <-errs:
			if err == nil || !strings.Contains(err.Error(), "host session ended") {
				t.Fatalf("pending control error = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("pending control was not failed on disconnect")
		}
	}
	<-serveDone
	waitControlWorkers(t, c)
	if got := calls.Load(); got != maxConcurrentControls {
		t.Fatalf("handler calls = %d, want only the %d running requests", got, maxConcurrentControls)
	}
}

// A guest that floods control frames without ever reading responses
// must not accumulate host goroutines: fixed workers, a bounded queue,
// and bounded refusal writers cap the work. The channel keeps working
// once the guest resumes reading.
func TestControlFloodBoundedWithoutReading(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	c := &Client{
		conn:    host,
		w:       newFrameWriter(host),
		r:       bufioReader(host),
		pending: map[string]chan Frame{},
		done:    make(chan struct{}),
	}
	const flood = 100
	entered := make(chan struct{}, flood)
	release := make(chan struct{})
	var active, maxActive atomic.Int32
	c.HandleControl(func(ctx context.Context, req *Frame) ([]byte, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			max := maxActive.Load()
			if current <= max || maxActive.CompareAndSwap(max, current) {
				break
			}
		}
		entered <- struct{}{}
		select {
		case <-release:
			return []byte(`{}`), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	go c.readLoop()

	floodErr := make(chan error, 1)
	go func() {
		w := newFrameWriter(guest)
		for i := 0; i < flood; i++ {
			if err := w.write(&Frame{ID: fmt.Sprintf("f%d", i), Op: OpControl, Action: ControlMapList}); err != nil {
				floodErr <- err
				return
			}
		}
		floodErr <- nil
	}()

	for i := 0; i < maxConcurrentControls; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d handlers admitted", i)
		}
	}
	if err := <-floodErr; err != nil {
		t.Fatalf("flood write: %v", err)
	}
	// Reading this non-control barrier means every earlier control frame
	// has completed admission while the queue and refusal writers are full.
	if err := newFrameWriter(guest).write(&Frame{ID: "barrier", Op: OpPing}); err != nil {
		t.Fatalf("flood barrier: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for (len(c.ctrlQueue) != maxQueuedControls || len(c.busySem) != maxConcurrentControlRefusals) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := len(c.ctrlQueue); got != maxQueuedControls {
		t.Fatalf("queued = %d, want %d", got, maxQueuedControls)
	}
	if got := len(c.busySem); got != maxConcurrentControlRefusals {
		t.Fatalf("refusal writers = %d, want %d", got, maxConcurrentControlRefusals)
	}
	if got := maxActive.Load(); got != maxConcurrentControls {
		t.Fatalf("concurrent handlers = %d, want %d", got, maxConcurrentControls)
	}

	accepted := maxConcurrentControls + maxQueuedControls
	expected := accepted + maxConcurrentControlRefusals
	close(release)
	r := bufio.NewReaderSize(guest, 64<<10)
	results, busy := 0, 0
	for i := 0; i < expected; i++ {
		var f Frame
		guest.SetReadDeadline(time.Now().Add(5 * time.Second))
		if err := readFrame(r, &f); err != nil {
			t.Fatalf("read response %d/%d: %v", i+1, expected, err)
		}
		switch {
		case f.Op == OpResult:
			results++
		case f.Error == "control busy":
			busy++
		default:
			t.Fatalf("unexpected flood response: %+v", f)
		}
	}
	if results != accepted || busy != maxConcurrentControlRefusals {
		t.Fatalf("results = %d, busy = %d; want %d and %d", results, busy, accepted, maxConcurrentControlRefusals)
	}
	if got := maxActive.Load(); got != maxConcurrentControls {
		t.Fatalf("peak concurrent handlers = %d, want %d", got, maxConcurrentControls)
	}

	// The next frame must answer a fresh request, not an extra queued or
	// refused flood request.
	if err := newFrameWriter(guest).write(&Frame{ID: "ok1", Op: OpControl, Action: ControlMapList}); err != nil {
		t.Fatal(err)
	}
	var f Frame
	guest.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := readFrame(r, &f); err != nil || f.Op != OpResult || f.ID != "ok1" {
		t.Fatalf("post-flood control: %v %+v", err, f)
	}
	c.Close()
	waitControlWorkers(t, c)
}

// A guest that never reads responses must have its connection torn
// down after the write deadline: the frameWriter latches the first
// error, so keeping the client would poison every later write. The
// redialed client (what the daemon's watcher builds) works immediately.
func TestControlWriteTimeoutKillsClientAndRedialWorks(t *testing.T) {
	host, guest := net.Pipe()
	c := &Client{
		conn:             host,
		w:                newFrameWriter(host),
		r:                bufioReader(host),
		pending:          map[string]chan Frame{},
		done:             make(chan struct{}),
		ctrlWriteTimeout: 100 * time.Millisecond,
	}
	c.HandleControl(func(ctx context.Context, req *Frame) ([]byte, error) {
		return []byte(`{"ok":true}`), nil
	})
	go c.readLoop()

	// The guest sends a request and never reads the response.
	if err := newFrameWriter(guest).write(&Frame{ID: "w1", Op: OpControl, Action: ControlMapList}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !c.Closed() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !c.Closed() {
		t.Fatal("client not marked dead after the response write timed out")
	}
	// The guest observes the teardown.
	guest.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := guest.Read(make([]byte, 1)); err == nil {
		t.Fatal("guest should see the connection closed")
	}
	guest.Close()
	waitControlWorkers(t, c)

	// The redialed client works immediately.
	host2, guest2 := net.Pipe()
	defer host2.Close()
	defer guest2.Close()
	c2 := &Client{
		conn:    host2,
		w:       newFrameWriter(host2),
		r:       bufioReader(host2),
		pending: map[string]chan Frame{},
		done:    make(chan struct{}),
	}
	c2.HandleControl(func(ctx context.Context, req *Frame) ([]byte, error) {
		return []byte(`{"ok":true}`), nil
	})
	go c2.readLoop()
	if err := newFrameWriter(guest2).write(&Frame{ID: "r1", Op: OpControl, Action: ControlMapList}); err != nil {
		t.Fatal(err)
	}
	var f Frame
	guest2.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := readFrame(bufio.NewReaderSize(guest2, 64<<10), &f); err != nil || f.Op != OpResult {
		t.Fatalf("redialed control: %v %+v", err, f)
	}
	c2.Close()
	waitControlWorkers(t, c2)
}

func waitControlWorkers(t *testing.T, c *Client) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		c.ctrlWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("control workers did not exit")
	}
}

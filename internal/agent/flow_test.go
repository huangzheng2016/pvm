package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestExecFlowControlLargeStdinAndEOF(t *testing.T) {
	_, c := pipePair(t)
	input := strings.Repeat("stdin-data\n", 400000)
	var output bytes.Buffer
	result, err := c.Exec([]string{"cat"}, strings.NewReader(input), &output, nil, 10)
	if err != nil || result.ExitCode != 0 || output.String() != input {
		t.Fatalf("stdin truncated: result=%v err=%v bytes=%d", result, err, output.Len())
	}
	result, err = c.Exec([]string{"cat"}, nil, nil, nil, 2)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("nil stdin did not EOF: %v %v", result, err)
	}
}

func TestSlowExecDoesNotBlockPingOrOtherExec(t *testing.T) {
	_, c := pipePair(t)
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var once sync.Once
	go func() {
		defer close(finished)
		c.ExecStreamContext(ctx, []string{"sh", "-c", "while :; do printf 'output data\\n'; done"}, nil, 10, func(f *Frame) {
			if f.Stream != "" {
				once.Do(func() { close(entered) })
				<-release
			}
		})
	}()
	defer func() { close(release); cancel(); <-finished }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("no output")
	}
	pingCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if _, err := c.PingContext(pingCtx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ExecStreamContext(pingCtx, []string{"true"}, nil, 1, nil); err != nil {
		t.Fatal(err)
	}
}

func TestExecCancellationUnregistersAndStopsGuest(t *testing.T) {
	srv, c := pipePair(t)
	var groups []int
	defer func() {
		for _, pgid := range groups {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
	}()
	for i := 0; i < 25; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		_, err := c.ExecStreamContext(ctx, []string{"sh", "-c", "echo $$; sleep 60"}, nil, 0, func(f *Frame) {
			data, _ := base64.StdEncoding.DecodeString(f.DataB64)
			if pgid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				groups = append(groups, pgid)
			}
			cancel()
		})
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		srv.mu.Lock()
		count := len(srv.execs)
		srv.mu.Unlock()
		alive := 0
		for _, pgid := range groups {
			if syscall.Kill(-pgid, 0) == nil {
				alive++
			}
		}
		if count == 0 && alive == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d guest execs and %d process groups remain", count, alive)
		}
		time.Sleep(time.Millisecond)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) != 0 {
		t.Fatalf("pending: %d", len(c.pending))
	}
}

func TestFailAllFullQueueAndConcurrentDelivery(t *testing.T) {
	for i := 0; i < 100; i++ {
		_, c := pipePair(t)
		ch := c.register("full")
		for j := 0; j < cap(ch); j++ {
			ch <- Frame{ID: "full"}
		}
		done := make(chan struct{})
		go func() { c.failAll(errors.New("disconnected")); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("failAll blocked")
		}
		if f := <-ch; f.Error == "" {
			t.Fatal("missing terminal error")
		}
		c.Close()
	}
}

func TestPingPendingCleanup(t *testing.T) {
	_, c := pipePair(t)
	for i := 0; i < 100; i++ {
		if _, err := c.Ping(); err != nil {
			t.Fatal(err)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) != 0 {
		t.Fatalf("pending: %d", len(c.pending))
	}
}

func TestCloseRacesResponseDelivery(t *testing.T) {
	for i := 0; i < 50; i++ {
		_, c := pipePair(t)
		var wg sync.WaitGroup
		for j := 0; j < 8; j++ {
			wg.Add(1)
			go func() { defer wg.Done(); c.Ping() }()
		}
		c.Close()
		finished := make(chan struct{})
		go func() { wg.Wait(); close(finished) }()
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Fatal("pending ping stuck after Close")
		}
	}
}

func TestCancelledWriteWaitUnregisters(t *testing.T) {
	_, c := pipePair(t)
	lockClientWrites(c)
	defer func() { <-c.writeGate }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.PingContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("ping: %v", err)
	}
	if _, err := c.ExecStreamContext(ctx, []string{"true"}, nil, 0, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("exec: %v", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) != 0 {
		t.Fatalf("pending: %d", len(c.pending))
	}
}

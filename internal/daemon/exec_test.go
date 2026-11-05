package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huangzheng2016/pvm/internal/agent"
)

type stalledExecWriter struct {
	header        http.Header
	entered       chan struct{}
	interrupted   chan struct{}
	once          sync.Once
	interruptOnce sync.Once
	stallFlush    bool
}

func (w *stalledExecWriter) Header() http.Header { return w.header }
func (w *stalledExecWriter) WriteHeader(int)     {}
func (w *stalledExecWriter) stall() error {
	w.once.Do(func() { close(w.entered) })
	<-w.interrupted
	return context.DeadlineExceeded
}
func (w *stalledExecWriter) Write(p []byte) (int, error) {
	if w.stallFlush {
		return len(p), nil
	}
	return 0, w.stall()
}
func (w *stalledExecWriter) FlushError() error { return w.stall() }
func (w *stalledExecWriter) SetWriteDeadline(d time.Time) error {
	if !d.IsZero() && !d.After(time.Now()) {
		w.interruptOnce.Do(func() { close(w.interrupted) })
	}
	return nil
}

func TestExecStreamCancellationInterruptsHTTP(t *testing.T) {
	for _, flush := range []bool{false, true} {
		name := "write"
		if flush {
			name = "flush"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w := &stalledExecWriter{header: make(http.Header), entered: make(chan struct{}), interrupted: make(chan struct{}), stallFlush: flush}
			done := make(chan struct{})
			go func() {
				defer close(done)
				streamExecResponse(ctx, w, func(ctx context.Context, frame func(*agent.Frame) error) error {
					return frame(&agent.Frame{Stream: "stdout", DataB64: "eA=="})
				})
			}()
			select {
			case <-w.entered:
			case <-time.After(time.Second):
				t.Fatal("HTTP output never started")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("cancellation left HTTP output blocked")
			}
		})
	}
}

func TestExecStreamTransportFailureIsNotReplayed(t *testing.T) {
	w := httptest.NewRecorder()
	attempts := 0
	streamExecResponse(context.Background(), w, func(context.Context, func(*agent.Frame) error) error {
		attempts++
		return errors.New("transport lost after dispatch")
	})
	if attempts != 1 {
		t.Fatalf("command dispatched %d times", attempts)
	}
	if !strings.Contains(w.Body.String(), "transport lost after dispatch") {
		t.Fatalf("missing failure: %s", w.Body.String())
	}
}

func TestExecStreamPassesRequestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	streamExecResponse(ctx, httptest.NewRecorder(), func(ctx context.Context, frame func(*agent.Frame) error) error {
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatal("request cancellation not propagated")
		}
		if !errors.Is(frame(&agent.Frame{Done: true}), context.Canceled) {
			t.Fatal("cancelled request attempted output")
		}
		return ctx.Err()
	})
}

type deadlineExecWriter struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (w *deadlineExecWriter) SetWriteDeadline(d time.Time) error {
	w.deadlines = append(w.deadlines, d)
	return nil
}
func TestExecStreamRefreshesWriteDeadlines(t *testing.T) {
	w := &deadlineExecWriter{ResponseRecorder: httptest.NewRecorder()}
	limit := time.Now().Add(time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), limit)
	defer cancel()
	streamExecResponse(ctx, w, func(ctx context.Context, frame func(*agent.Frame) error) error {
		for i := 0; i < 2; i++ {
			if err := frame(&agent.Frame{Stream: "stdout", DataB64: "eA=="}); err != nil {
				return err
			}
		}
		return nil
	})
	if len(w.deadlines) != 3 {
		t.Fatalf("got %d deadlines, want two writes and cleanup", len(w.deadlines))
	}
	for _, d := range w.deadlines[:2] {
		if !d.Equal(limit) {
			t.Fatalf("write deadline %s does not preserve request deadline %s", d, limit)
		}
	}
	if !w.deadlines[2].IsZero() {
		t.Fatal("write deadline leaked into next request")
	}
}

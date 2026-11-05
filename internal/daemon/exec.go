package daemon

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/huangzheng2016/pvm/internal/agent"
)

// handleExec bridges an HTTP request to the guest agent's virtio-serial
// channel. Request: JSON {cmd:[...], timeout_s, stdin_b64}. Response:
// ndjson stream of agent frames ({stream,data_b64}...{done,exit_code}),
// so callers see output as it happens.
func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req struct {
		Cmd      []string `json:"cmd"`
		TimeoutS int      `json:"timeout_s"`
		StdinB64 string   `json:"stdin_b64"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if len(req.Cmd) == 0 {
		writeErr(w, 400, fmt.Errorf("empty cmd"))
		return
	}
	spec, err := s.getSpec(name)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	if st, err := s.instance(spec).Running(); err != nil {
		writeErr(w, 500, err)
		return
	} else if st == nil {
		writeErr(w, 409, fmt.Errorf("vm %s is not running", name))
		return
	}

	var stdinRaw []byte
	if req.StdinB64 != "" {
		raw, derr := base64.StdEncoding.DecodeString(req.StdinB64)
		if derr != nil {
			writeErr(w, 400, fmt.Errorf("bad stdin_b64: %v", derr))
			return
		}
		stdinRaw = raw
	}

	if req.TimeoutS <= 0 {
		req.TimeoutS = 600
	}
	if req.TimeoutS > int((time.Duration(1<<63-1)-time.Minute)/time.Second) {
		writeErr(w, 400, fmt.Errorf("timeout_s is too large"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(req.TimeoutS)*time.Second+time.Minute)
	defer cancel()
	ac, err := s.agentClientContext(ctx, name)
	if err != nil {
		writeErr(w, 502, err)
		return
	}
	var stdin io.Reader
	if stdinRaw != nil {
		stdin = bytes.NewReader(stdinRaw)
	}
	// Once dispatched, a command may have run even when no frame arrives.
	// Never replay it after a transport failure.
	streamExecResponse(ctx, w, func(ctx context.Context, onFrame func(*agent.Frame) error) error {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		var frameErr error
		_, err := ac.ExecStreamContext(ctx, req.Cmd, stdin, req.TimeoutS, func(f *agent.Frame) {
			if frameErr == nil {
				frameErr = onFrame(f)
				if frameErr != nil {
					cancel()
				}
			}
		})
		if frameErr != nil {
			return frameErr
		}
		if err != nil {
			return fmt.Errorf("exec result unknown; command was not retried: %w", err)
		}
		return nil
	})
}

func streamExecResponse(ctx context.Context, w http.ResponseWriter, execute func(context.Context, func(*agent.Frame) error) error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	rc := http.NewResponseController(w)
	// Interrupt a blocked HTTP write when its request is cancelled.
	var deadlineMu sync.Mutex
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(stopped)
		deadlineMu.Lock()
		defer deadlineMu.Unlock()
		_ = rc.SetWriteDeadline(time.Now())
	})
	defer func() {
		if !stop() {
			<-stopped
		}
		_ = rc.SetWriteDeadline(time.Time{})
	}()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	write := func(value any) error {
		deadlineMu.Lock()
		if err := ctx.Err(); err != nil {
			deadlineMu.Unlock()
			return err
		}
		deadline := time.Now().Add(15 * time.Second)
		if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
			deadline = limit
		}
		_ = rc.SetWriteDeadline(deadline)
		deadlineMu.Unlock()
		if err := enc.Encode(value); err != nil {
			cancel()
			return err
		}
		if err := rc.Flush(); err != nil && err != http.ErrNotSupported {
			cancel()
			return err
		}
		return nil
	}
	if err := execute(ctx, func(f *agent.Frame) error { return write(f) }); err != nil && ctx.Err() == nil {
		_ = write(map[string]any{"error": err.Error()})
	}
}

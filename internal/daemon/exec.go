package daemon

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(200)
	flush := func() {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)

	// One retry on a stale shared connection (VM rebooted, agent
	// restarted): drop and redial once before giving up. A hard
	// deadline (guest timeout + grace) guarantees the handler exits
	// even if the guest loses a frame.
	if req.TimeoutS <= 0 {
		req.TimeoutS = 600
	}
	var execErr error
	for attempt := 0; attempt < 2; attempt++ {
		ac, err := s.agentClient(name)
		if err != nil {
			execErr = err
			time.Sleep(500 * time.Millisecond)
			continue
		}
		var stdin io.Reader
		if stdinRaw != nil {
			stdin = bytes.NewReader(stdinRaw)
		}
		sawFrame := false
		done := make(chan error, 1)
		go func() {
			_, err := ac.ExecStream(req.Cmd, stdin, req.TimeoutS, func(f *agent.Frame) {
				sawFrame = true
				enc.Encode(f)
				flush()
			})
			done <- err
		}()
		select {
		case execErr = <-done:
		case <-time.After(time.Duration(req.TimeoutS)*time.Second + 60*time.Second):
			execErr = fmt.Errorf("exec hard deadline exceeded")
			s.dropAgent(name, ac) // unblocks the goroutine via closed conn
			<-done
		}
		if execErr == nil {
			return
		}
		// Only a failure before any frame came back is retried: once the
		// guest may have started executing, a replay would run the command
		// twice. Hard deadlines always fall into that no-retry class.
		if !errors.Is(execErr, agent.ErrUnreachable) || sawFrame {
			break
		}
		s.dropAgent(name, ac)
	}
	if execErr != nil {
		enc.Encode(map[string]any{"error": execErr.Error()})
		flush()
	}
}

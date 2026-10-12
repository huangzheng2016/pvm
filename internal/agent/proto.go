// Package agent implements the guest-side pvm agent and its host-side
// client. The transport is a QEMU virtio-serial port (host end is a unix
// socket), so the channel works with no guest network at all.
//
// Wire format: newline-delimited JSON frames in both directions.
//
//	host -> guest: {id, op:"ping"}
//	host -> guest: {id, op:"exec", cmd:["/bin/echo","ok"], timeout_s:30}
//	host -> guest: {id, op:"stdin", data_b64:"..."}   (zero or more)
//	host -> guest: {id, op:"stdin", eof:true}         (closes stdin)
//	guest -> host: {id, stream:"stdout"|"stderr", data_b64:"..."}
//	guest -> host: {id, done:true, exit_code:0}       (terminal frame)
//	guest -> host: {id, op:"pong", version, hostname, uptime_s}
//	guest -> host: {id, error:"..."}                  (terminal frame)
package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

const (
	PortName = "pvm.agent"

	OpPing  = "ping"
	OpPong  = "pong"
	OpExec  = "exec"
	OpStdin = "stdin"

	StreamStdout = "stdout"
	StreamStderr = "stderr"
)

// Frame is a single protocol message in either direction; which fields
// are meaningful depends on Op / the frame role.
type Frame struct {
	ID       string   `json:"id"`
	Op       string   `json:"op,omitempty"`
	Cmd      []string `json:"cmd,omitempty"`
	TimeoutS int      `json:"timeout_s,omitempty"`

	Stream  string `json:"stream,omitempty"`
	DataB64 string `json:"data_b64,omitempty"`
	EOF     bool   `json:"eof,omitempty"`

	Done     bool   `json:"done,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`

	Version  string `json:"version,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	UptimeS  int64  `json:"uptime_s,omitempty"`
}

// MaxFrameBytes bounds one JSON line; data frames carry at most ~3 MiB
// of decoded payload.
const MaxFrameBytes = 4 << 20

type frameWriter struct {
	w   *bufio.Writer
	err error
}

func newFrameWriter(w io.Writer) *frameWriter {
	return &frameWriter{w: bufio.NewWriterSize(w, 64<<10)}
}

func (fw *frameWriter) write(f *Frame) error {
	if fw.err != nil {
		return fw.err
	}
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := fw.w.Write(data); err == nil {
		err = fw.w.Flush()
	}
	fw.err = err
	return err
}

func readFrame(r *bufio.Reader, f *Frame) error {
	line, err := r.ReadBytes('\n')
	if err != nil {
		if err == io.EOF && len(line) > 0 {
			// tolerate a final unterminated frame
		} else {
			return err
		}
	}
	if len(line) > MaxFrameBytes {
		return fmt.Errorf("frame too large (%d bytes)", len(line))
	}
	if err := json.Unmarshal(line, f); err != nil {
		return fmt.Errorf("bad frame: %w", err)
	}
	if f.ID == "" {
		return fmt.Errorf("frame without id")
	}
	return nil
}

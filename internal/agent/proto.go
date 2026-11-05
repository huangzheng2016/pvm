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
//	guest -> host: {id, op:"stdin_ack"}            (after consuming stdin)
//	guest -> host: {id, stream:"stdout"|"stderr", data_b64:"..."}
//	host -> guest: {id, op:"output_ack"}           (after consuming output)
//	host -> guest: {id, op:"cancel"}               (kills the exec process group)
//	guest -> host: {id, done:true, exit_code:0}       (terminal frame)
//	guest -> host: {id, op:"pong", version, hostname, uptime_s}
//	guest -> host: {id, error:"..."}                  (terminal frame)
//	guest -> host: {id, op:"control", action:"map.list", payload:{...}}
//	host -> guest: {id, op:"result", payload:{...}}   (terminal frame)
//
// Each stdin sender waits for its acknowledgement before sending more data;
// stdout and stderr each allow one outstanding frame. The shared read loop
// never waits on an individual command's input or output consumer.
//
// Control frames are guest-initiated and scoped by the host to the VM
// owning the channel; the guest never names a VM. OpExec is never
// accepted from the guest.
package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

const (
	PortName = "pvm.agent"

	OpPing      = "ping"
	OpPong      = "pong"
	OpExec      = "exec"
	OpStdin     = "stdin"
	OpStdinAck  = "stdin_ack"
	OpOutputAck = "output_ack"
	OpCancel    = "cancel"
	OpControl   = "control"
	OpResult    = "result"

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

	Action  string          `json:"action,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`

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
	if _, err := fw.w.Write(data); err != nil {
		fw.err = err
		return err
	}
	// Flush failures must latch too: a swallowed flush error leaves the
	// caller thinking the frame landed and poisons nothing, while the
	// sticky writer keeps "succeeding" on a dead channel.
	if err := fw.w.Flush(); err != nil {
		fw.err = err
		return err
	}
	return nil
}

func readFrame(r *bufio.Reader, f *Frame) error {
	var line []byte
	for {
		part, err := r.ReadSlice('\n')
		if len(part) > MaxFrameBytes-len(line) {
			return fmt.Errorf("frame too large (limit %d bytes)", MaxFrameBytes)
		}
		line = append(line, part...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil && (err != io.EOF || len(line) == 0) {
			return err
		}
		break // tolerate a final unterminated frame
	}
	if err := json.Unmarshal(line, f); err != nil {
		return fmt.Errorf("bad frame: %w", err)
	}
	if f.ID == "" {
		return fmt.Errorf("frame without id")
	}
	return nil
}

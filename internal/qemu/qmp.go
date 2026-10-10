package qemu

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// QMP is a minimal QEMU Machine Protocol client: enough for capability
// negotiation, status query and graceful powerdown.
type QMP struct {
	conn net.Conn
	r    *bufio.Reader
}

type qmpGreeting struct {
	QMP struct {
		Version struct {
			QEMU struct {
				Major int `json:"major"`
				Minor int `json:"minor"`
				Micro int `json:"micro"`
			} `json:"qemu"`
		} `json:"version"`
	} `json:"QMP"`
}

type qmpResponse struct {
	Return json.RawMessage `json:"return"`
	Error  *struct {
		Class string `json:"class"`
		Desc  string `json:"desc"`
	} `json:"error"`
}

func DialQMP(sockPath string) (*QMP, error) {
	conn, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		return nil, err
	}
	q := &QMP{conn: conn, r: bufio.NewReader(conn)}
	var g qmpGreeting
	if err := q.readJSON(&g); err != nil {
		conn.Close()
		return nil, fmt.Errorf("qmp greeting: %w", err)
	}
	if _, err := q.Execute("qmp_capabilities", nil); err != nil {
		conn.Close()
		return nil, fmt.Errorf("qmp capabilities: %w", err)
	}
	return q, nil
}

func (q *QMP) Close() { q.conn.Close() }

func (q *QMP) readJSON(v any) error {
	q.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := q.r.ReadBytes('\n')
	if err != nil {
		return err
	}
	return json.Unmarshal(line, v)
}

// Execute runs one command and returns the raw "return" payload. Async
// events interleaved between responses are skipped.
func (q *QMP) Execute(cmd string, args map[string]any) (json.RawMessage, error) {
	req := map[string]any{"execute": cmd}
	if args != nil {
		req["arguments"] = args
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	q.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := q.conn.Write(append(data, '\r', '\n')); err != nil {
		return nil, err
	}
	for {
		var resp qmpResponse
		line, err := q.r.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(line, &resp); err != nil {
			return nil, err
		}
		if resp.Return == nil && resp.Error == nil {
			continue // async event
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("qmp %s: %s: %s", cmd, resp.Error.Class, resp.Error.Desc)
		}
		return resp.Return, nil
	}
}

// Powerdown asks the guest to shut down via ACPI.
func (q *QMP) Powerdown() error {
	_, err := q.Execute("system_powerdown", nil)
	return err
}

// Status returns e.g. "running", "paused", "shutdown".
func (q *QMP) Status() (string, error) {
	raw, err := q.Execute("query-status", nil)
	if err != nil {
		return "", err
	}
	var s struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", err
	}
	return s.Status, nil
}

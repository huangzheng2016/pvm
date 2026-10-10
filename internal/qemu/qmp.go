package qemu

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"
)

// QMP is a minimal QEMU Machine Protocol client: enough for capability
// negotiation, status query and graceful powerdown.
type QMP struct {
	conn   net.Conn
	r      *bufio.Reader
	events []string
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
// events interleaved between responses are buffered for WaitEvent.
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
		line, err := q.r.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		var resp qmpResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			return nil, err
		}
		if resp.Return == nil && resp.Error == nil {
			var ev struct {
				Event string `json:"event"`
			}
			json.Unmarshal(line, &ev)
			if ev.Event != "" {
				q.events = append(q.events, ev.Event)
			}
			continue
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("qmp %s: %s: %s", cmd, resp.Error.Class, resp.Error.Desc)
		}
		return resp.Return, nil
	}
}

// WaitEvent blocks until the named QMP event arrives (already-buffered
// events count) or the timeout expires.
func (q *QMP) WaitEvent(name string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		for i, e := range q.events {
			if e == name {
				q.events = append(q.events[:i], q.events[i+1:]...)
				return nil
			}
		}
		remain := time.Until(deadline)
		if remain <= 0 {
			return fmt.Errorf("timeout waiting for %s", name)
		}
		q.conn.SetReadDeadline(time.Now().Add(remain))
		line, err := q.r.ReadBytes('\n')
		if err != nil {
			return err
		}
		var ev struct {
			Event string `json:"event"`
		}
		json.Unmarshal(line, &ev)
		if ev.Event != "" {
			q.events = append(q.events, ev.Event)
		}
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

// NetdevAddStream hot-adds a listening stream netdev (gateway side of a
// cluster link). A duplicate id is treated as success so link ensure is
// idempotent.
func (q *QMP) NetdevAddStream(id, sockPath string) error {
	return q.netdevAddStream(id, sockPath, true)
}

// NetdevAddStreamConnect hot-adds a client stream netdev (user VM side
// of a cluster link, used when repairing after gateway reboot).
func (q *QMP) NetdevAddStreamConnect(id, sockPath string) error {
	return q.netdevAddStream(id, sockPath, false)
}

func (q *QMP) netdevAddStream(id, sockPath string, server bool) error {
	_, err := q.Execute("netdev_add", map[string]any{
		"type":   "stream",
		"id":     id,
		"server": server,
		"addr":   map[string]any{"type": "unix", "path": sockPath},
	})
	if err != nil && strings.Contains(err.Error(), "uplicate") {
		return nil
	}
	return err
}

func (q *QMP) NetdevDel(id string) error {
	_, err := q.Execute("netdev_del", map[string]any{"id": id})
	return err
}

// DeviceAddNet hot-plugs a virtio NIC into the given pcie-root-port.
func (q *QMP) DeviceAddNet(id, netdevID, mac, bus string) error {
	args := map[string]any{
		"driver": "virtio-net-pci",
		"id":     id,
		"netdev": netdevID,
	}
	if mac != "" {
		args["mac"] = mac
	}
	if bus != "" {
		args["bus"] = bus
	}
	_, err := q.Execute("device_add", args)
	return err
}

// DeviceDel requests device removal; completion is asynchronous but the
// netdev can be deleted right after for our virtio-nic use.
func (q *QMP) DeviceDel(id string) error {
	_, err := q.Execute("device_del", map[string]any{"id": id})
	return err
}

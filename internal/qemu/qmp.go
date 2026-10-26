package qemu

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

type QMP struct {
	mu     sync.Mutex
	conn   net.Conn
	r      *bufio.Reader
	events []qmpEvent
}

// qmpEvent is a buffered async event; device carries the DEVICE_DELETED
// device id ("" for other events).
type qmpEvent struct {
	name   string
	device string
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

type qmpCommandError struct{ error }

func (q *QMP) closeOnProtocolError(err error) {
	var commandError qmpCommandError
	if err != nil && !errors.As(err, &commandError) {
		q.conn.Close()
	}
}

type qmpResponse struct {
	Return json.RawMessage `json:"return"`
	Error  *struct {
		Class string `json:"class"`
		Desc  string `json:"desc"`
	} `json:"error"`
}

func DialQMP(sockPath string) (*QMP, error) {
	if err := validateUnixSocketPath("QMP", sockPath); err != nil {
		return nil, err
	}
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
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.execute(cmd, args)
}

func (q *QMP) execute(cmd string, args map[string]any) (json.RawMessage, error) {
	return q.executeUntil(cmd, args, time.Now().Add(60*time.Second))
}

func (q *QMP) executeUntil(cmd string, args map[string]any, deadline time.Time) (result json.RawMessage, retErr error) {
	defer func() { q.closeOnProtocolError(retErr) }()
	req := map[string]any{"execute": cmd}
	if args != nil {
		req["arguments"] = args
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	writeDeadline := time.Now().Add(5 * time.Second)
	if deadline.Before(writeDeadline) {
		writeDeadline = deadline
	}
	q.conn.SetWriteDeadline(writeDeadline)
	if _, err := q.conn.Write(append(data, '\r', '\n')); err != nil {
		return nil, err
	}
	q.conn.SetReadDeadline(deadline)
	defer q.conn.SetReadDeadline(time.Time{})
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
			var ev struct {
				Event string `json:"event"`
				Data  struct {
					Device string `json:"device"`
				} `json:"data"`
			}
			json.Unmarshal(line, &ev)
			if ev.Event != "" {
				q.events = append(q.events, qmpEvent{name: ev.Event, device: ev.Data.Device})
			}
			continue
		}
		if resp.Error != nil {
			return nil, qmpCommandError{fmt.Errorf("qmp %s: %s: %s", cmd, resp.Error.Class, resp.Error.Desc)}
		}
		return resp.Return, nil
	}
}

// WaitEvent blocks until the named QMP event arrives (already-buffered
// events count) or the timeout expires.
func (q *QMP) WaitEvent(name string, d time.Duration) (retErr error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	defer func() { q.closeOnProtocolError(retErr) }()
	defer q.conn.SetReadDeadline(time.Time{})
	deadline := time.Now().Add(d)
	for {
		for i, e := range q.events {
			if e.name == name {
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
			Data  struct {
				Device string `json:"device"`
			} `json:"data"`
		}
		if err := json.Unmarshal(line, &ev); err != nil {
			return err
		}
		if ev.Event != "" {
			q.events = append(q.events, qmpEvent{name: ev.Event, device: ev.Data.Device})
		}
	}
}

func (q *QMP) Powerdown() error {
	_, err := q.Execute("system_powerdown", nil)
	return err
}

func (q *QMP) Stop() error {
	_, err := q.Execute("stop", nil)
	return err
}

func (q *QMP) Cont() error {
	_, err := q.Execute("cont", nil)
	return err
}

func (q *QMP) Quit() error {
	_, err := q.Execute("quit", nil)
	return err
}

func (q *QMP) Migrate(uri string) error {
	_, err := q.Execute("migrate", map[string]any{"uri": uri})
	return err
}

// DeviceDel hot-unplugs a device and waits for the DEVICE_DELETED event
// (guest PCI hot-unplug completion).
func (q *QMP) DeviceDel(id string, timeout time.Duration) (retErr error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	defer func() { q.closeOnProtocolError(retErr) }()
	deadline := time.Now().Add(timeout)
	if _, err := q.executeUntil("device_del", map[string]any{"id": id}, deadline); err != nil {
		return err
	}
	defer q.conn.SetReadDeadline(time.Time{}) // don't leak the deadline into later commands
	for {
		// Drain buffered events first: DEVICE_DELETED may already have
		// arrived during an earlier Execute.
		for i, e := range q.events {
			if e.name == "DEVICE_DELETED" && e.device == id {
				q.events = append(q.events[:i], q.events[i+1:]...)
				return nil
			}
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("device_del %s: timed out waiting for DEVICE_DELETED", id)
		}
		q.conn.SetReadDeadline(deadline)
		var msg struct {
			Event string `json:"event"`
			Data  struct {
				Device string `json:"device"`
			} `json:"data"`
		}
		line, err := q.r.ReadBytes('\n')
		if err != nil {
			return fmt.Errorf("device_del %s: %w", id, err)
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			return err
		}
		if msg.Event == "DEVICE_DELETED" && msg.Data.Device == id {
			return nil
		}
		// Buffer unrelated events for later WaitEvent/DeviceDel callers
		// instead of silently dropping them.
		if msg.Event != "" {
			q.events = append(q.events, qmpEvent{name: msg.Event, device: msg.Data.Device})
		}
	}
}

func (q *QMP) MigrateStatus() (string, error) {
	raw, err := q.Execute("query-migrate", nil)
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
	if err := validateUnixSocketPath("network link", sockPath); err != nil {
		return err
	}
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

// DeviceAddVirtFS attaches a front-end to a pre-created 9p backend.
func (q *QMP) DeviceAddVirtFS(slot int, tag string) error {
	id := VirtFSID(slot)
	_, err := q.Execute("device_add", map[string]any{
		"driver":    "virtio-9p-pci-non-transitional",
		"id":        id,
		"fsdev":     id,
		"mount_tag": tag,
		"bus":       VirtFSRootPort(slot),
	})
	return err
}

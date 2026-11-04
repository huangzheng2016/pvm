package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// ControlSock is the guest-local unix socket the agent broker listens
// on. Any local user may manage this VM's own maps; the host scopes
// every request to the VM owning the virtio channel.
const ControlSock = "/run/pvm/control.sock"

// MaxControlPayloadBytes bounds a control request or result payload.
const MaxControlPayloadBytes = 64 << 10

// maxConcurrentControls caps concurrently running host handlers per
// agent connection.
const maxConcurrentControls = 8

// controlCallTimeout bounds one brokered control round trip. The host
// side bounds its own work (ssh/gateway push timeouts), so this only
// guards a silent channel.
const controlCallTimeout = 30 * time.Second

// controlHandlerTimeout bounds one host-side control handler: the guest
// broker gives up at controlCallTimeout, so the host stops earlier and
// its error still reaches the guest. It is a variable so tests can
// shorten it.
var controlHandlerTimeout = 25 * time.Second

// Control actions a guest may request; the host rejects anything else.
const (
	ControlMapList   = "map.list"
	ControlMapAdd    = "map.add"
	ControlMapRemove = "map.remove"
)

// MapEntry is the guest-visible shape of one port mapping.
type MapEntry struct {
	HostPort  int    `json:"host_port"`
	GuestPort int    `json:"guest_port"`
	Proto     string `json:"proto"`
}

type MapListResponse struct {
	Maps []MapEntry `json:"maps"`
}

// MapAddRequest mirrors the host map API: HostPort 0 asks the host to
// pick a free loopback port.
type MapAddRequest struct {
	HostPort  int    `json:"host_port"`
	GuestPort int    `json:"guest_port"`
	Proto     string `json:"proto,omitempty"`
}

type MapAddResponse struct {
	Map MapEntry `json:"map"`
}

type MapRemoveRequest struct {
	HostPort int    `json:"host_port"`
	Proto    string `json:"proto,omitempty"`
}

// ControlHandler answers a guest-initiated control request; the
// returned payload becomes the result frame. The host binds the VM
// identity to the channel, never to request fields. The context carries
// an overall deadline and is cancelled when the connection drops.
type ControlHandler func(ctx context.Context, req *Frame) ([]byte, error)

func newControlID() string {
	return "c" + newID()
}

// ServeControl accepts local connections on sock and brokers one
// control request each to the host over the active virtio session. It
// returns when the listener fails.
func (s *Server) ServeControl(sock string) error {
	if dir := filepath.Dir(sock); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	// Every local user may manage this VM's own mappings.
	os.Chmod(sock, 0o666)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.serveControlConn(conn)
	}
}

func (s *Server) serveControlConn(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(controlCallTimeout + 10*time.Second))
	var f Frame
	if err := readFrame(bufio.NewReaderSize(conn, 64<<10), &f); err != nil {
		return // malformed local request: drop the connection
	}
	w := newFrameWriter(conn)
	if f.Op != OpControl || f.Action == "" {
		w.write(&Frame{ID: f.ID, Error: "not a control request"})
		return
	}
	if len(f.Payload) > MaxControlPayloadBytes {
		w.write(&Frame{ID: f.ID, Error: "control payload too large"})
		return
	}
	payload, err := s.Control(f.Action, f.Payload, controlCallTimeout)
	if err != nil {
		w.write(&Frame{ID: f.ID, Error: err.Error()})
		return
	}
	w.write(&Frame{ID: f.ID, Op: OpResult, Payload: payload})
}

// ControlCall performs one control request against the guest-local
// agent broker: dial, one request frame, one response frame, close.
func ControlCall(sock, action string, req, resp any, timeout time.Duration) error {
	conn, err := net.DialTimeout("unix", sock, 3*time.Second)
	if err != nil {
		return fmt.Errorf("agent broker: %w", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))
	var payload json.RawMessage
	if req != nil {
		payload, err = json.Marshal(req)
		if err != nil {
			return err
		}
	}
	id := newControlID()
	if err := newFrameWriter(conn).write(&Frame{ID: id, Op: OpControl, Action: action, Payload: payload}); err != nil {
		return err
	}
	var f Frame
	if err := readFrame(bufio.NewReaderSize(conn, 64<<10), &f); err != nil {
		return err
	}
	if f.ID != id {
		return fmt.Errorf("broker response id mismatch")
	}
	if f.Error != "" {
		return errors.New(f.Error)
	}
	if resp != nil && len(f.Payload) > 0 {
		return json.Unmarshal(f.Payload, resp)
	}
	return nil
}

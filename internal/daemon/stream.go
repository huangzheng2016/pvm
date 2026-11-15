package daemon

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/huangzheng2016/pvm/internal/stream"
)

// streamTargets are the guest targets the host API may open. The guest
// keeps its own allowlist; this one only keeps the API surface explicit.
var streamTargets = map[string]bool{"echo": true, "discard": true, "source": true}

// streamState holds the one pvm.stream session per VM: QEMU's chardev
// socket accepts a single host connection, so every stream to a VM
// multiplexes over it.
type streamState struct {
	mu       sync.Mutex
	sessions map[string]*stream.Session
	dialing  map[string]chan struct{}
}

func (s *Server) streamSession(ctx context.Context, name string) (*stream.Session, error) {
	st := &s.streamState
	for {
		st.mu.Lock()
		if sess := st.sessions[name]; sess != nil && sess.Err() == nil {
			st.mu.Unlock()
			return sess, nil
		}
		wait := st.dialing[name]
		if wait == nil {
			if st.dialing == nil {
				st.dialing = make(map[string]chan struct{})
			}
			st.dialing[name] = make(chan struct{})
			st.mu.Unlock()
			break
		}
		st.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	sess, err := s.dialStream(ctx, name)
	st.mu.Lock()
	close(st.dialing[name])
	delete(st.dialing, name)
	if err == nil {
		if st.sessions == nil {
			st.sessions = make(map[string]*stream.Session)
		}
		st.sessions[name] = sess
	}
	st.mu.Unlock()
	return sess, err
}

func (s *Server) dialStream(ctx context.Context, name string) (*stream.Session, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	network, addr := "unix", s.P.StreamSock(name)
	if after, ok := strings.CutPrefix(addr, "tcp:"); ok {
		network, addr = "tcp", after
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, fmt.Errorf("%s of %s unreachable (a VM started before it existed needs `pvm stop %s && pvm start %s`): %w", stream.PortName, name, name, name, err)
	}
	return stream.Client(ctx, conn)
}

// dropStream ends the VM's stream session and every stream on it.
func (s *Server) dropStream(name string) {
	st := &s.streamState
	st.mu.Lock()
	sess := st.sessions[name]
	delete(st.sessions, name)
	st.mu.Unlock()
	if sess != nil {
		sess.Close()
	}
}

// handleStream upgrades the request to a raw byte stream connected to a
// guest target.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	spec, err := s.getSpec(name)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	target := r.URL.Query().Get("target")
	if !streamTargets[target] {
		writeErr(w, 400, fmt.Errorf("unknown stream target %q", target))
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "pvm-stream") {
		writeErr(w, 400, fmt.Errorf("stream requests must ask for Upgrade: pvm-stream"))
		return
	}
	if st, err := s.instance(spec).Running(); err != nil || st == nil {
		writeErr(w, 409, fmt.Errorf("vm %s is not running", name))
		return
	}
	sess, err := s.streamSession(r.Context(), name)
	if err != nil {
		writeErr(w, 502, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	str, err := sess.Open(ctx, target)
	cancel()
	if err != nil {
		writeErr(w, 502, err)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		str.Close()
		writeErr(w, 500, fmt.Errorf("connection cannot be upgraded"))
		return
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		str.Close()
		return
	}
	conn.SetDeadline(time.Time{})
	if _, err := buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: pvm-stream\r\n\r\n"); err != nil || buf.Flush() != nil {
		str.Close()
		conn.Close()
		return
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	stream.Pipe(str, &hijackedConn{Conn: conn, r: buf.Reader})
}

// hijackedConn reads through the server's buffer, which may already hold
// bytes the client sent after the request.
type hijackedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *hijackedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c *hijackedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Conn.Close()
}

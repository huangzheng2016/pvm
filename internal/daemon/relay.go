package daemon

import (
	"fmt"
	"net"
	"sync"
	"time"
)

// relaySet holds host-side listeners forwarding HostPort to
// 127.0.0.1:PoolPort (the gateway slirp hostfwd). It lets users pick any
// host port while the gateway keeps one small pre-mapped pool.
type relaySet struct {
	mu   sync.Mutex
	tcp  map[string]net.Listener
	udp  map[string]*udpRelay
	pool map[string]int
	bind map[string]string
}

func newRelaySet() *relaySet {
	return &relaySet{tcp: map[string]net.Listener{}, udp: map[string]*udpRelay{}, pool: map[string]int{}, bind: map[string]string{}}
}

func relayKey(proto string, port int) string { return fmt.Sprintf("%s/%d", proto, port) }

func (r *relaySet) Ensure(proto string, hostPort, poolPort int) error {
	return r.ensure(proto, hostPort, poolPort, "127.0.0.1")
}

func (r *relaySet) EnsurePublic(proto string, hostPort, poolPort int) error {
	if proto != "tcp" {
		return fmt.Errorf("public relay supports only tcp")
	}
	return r.ensure(proto, hostPort, poolPort, "0.0.0.0")
}

// An orphaned relay must never quietly serve a new VM's traffic.
func (r *relaySet) ensure(proto string, hostPort, poolPort int, bind string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := relayKey(proto, hostPort)
	if pp, ok := r.pool[key]; ok {
		if pp != poolPort {
			return fmt.Errorf("host port %d/%s already relayed to pool port %d (want %d)", hostPort, proto, pp, poolPort)
		}
		if r.bind[key] != bind {
			return fmt.Errorf("host port %d/%s is already relayed on %s", hostPort, proto, r.bind[key])
		}
		return nil
	}
	if proto == "udp" {
		rel, err := startUDPRelay(hostPort, poolPort)
		if err != nil {
			return err
		}
		r.udp[key] = rel
		r.pool[key] = poolPort
		r.bind[key] = bind
		return nil
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", bind, hostPort))
	if err != nil {
		return fmt.Errorf("relay listen %s:%d: %w", bind, hostPort, err)
	}
	r.tcp[key] = ln
	r.pool[key] = poolPort
	r.bind[key] = bind
	go acceptTCP(ln, poolPort)
	return nil
}

func (r *relaySet) Remove(proto string, hostPort int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := relayKey(proto, hostPort)
	if ln, ok := r.tcp[key]; ok {
		ln.Close()
		delete(r.tcp, key)
	}
	if rel, ok := r.udp[key]; ok {
		rel.close()
		delete(r.udp, key)
	}
	delete(r.pool, key)
	delete(r.bind, key)
}

func acceptTCP(ln net.Listener, poolPort int) {
	for {
		src, err := ln.Accept()
		if err != nil {
			return
		}
		go proxyTCP(src, poolPort)
	}
}

func proxyTCP(src net.Conn, poolPort int) {
	dst, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", poolPort), 10*time.Second)
	if err != nil {
		src.Close()
		return
	}
	// bidirectional copy preserving half-close semantics
	go func() {
		copyWithIdleTimeout(dst, src, relayIdleTimeout)
		if tc, ok := dst.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()
	copyWithIdleTimeout(src, dst, relayIdleTimeout)
	src.Close()
	dst.Close()
}

// Per-source sessions let UDP replies return to the original sender.
type udpRelay struct {
	conn     *net.UDPConn
	poolPort int
	mu       sync.Mutex
	sessions map[string]*udpSession
	closed   chan struct{}
	once     sync.Once
}

type udpSession struct {
	dst    *net.UDPConn
	lastAt time.Time
}

func startUDPRelay(hostPort, poolPort int) (*udpRelay, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: hostPort})
	if err != nil {
		return nil, fmt.Errorf("udp relay listen %d: %w", hostPort, err)
	}
	r := &udpRelay{conn: conn, poolPort: poolPort, sessions: map[string]*udpSession{}, closed: make(chan struct{})}
	go r.serve()
	go r.reaper()
	return r, nil
}

func (r *udpRelay) serve() {
	buf := make([]byte, 65535)
	for {
		n, src, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		r.dispatch(src, pkt)
	}
}

func (r *udpRelay) dispatch(src *net.UDPAddr, pkt []byte) {
	key := src.String()
	r.mu.Lock()
	sess, ok := r.sessions[key]
	if !ok {
		dst, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: r.poolPort})
		if err != nil {
			r.mu.Unlock()
			return
		}
		sess = &udpSession{dst: dst, lastAt: time.Now()}
		r.sessions[key] = sess
		go r.pumpBack(src, sess)
	}
	sess.lastAt = time.Now()
	r.mu.Unlock()
	sess.dst.Write(pkt)
}

func (r *udpRelay) pumpBack(src *net.UDPAddr, sess *udpSession) {
	buf := make([]byte, 65535)
	for {
		sess.dst.SetReadDeadline(time.Now().Add(2 * time.Minute))
		n, err := sess.dst.Read(buf)
		if err != nil {
			return
		}
		r.mu.Lock()
		sess.lastAt = time.Now()
		r.mu.Unlock()
		if _, err := r.conn.WriteToUDP(buf[:n], src); err != nil {
			return
		}
	}
}

func (r *udpRelay) reaper() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-r.closed:
			return
		case <-t.C:
		}
		r.mu.Lock()
		for k, s := range r.sessions {
			if time.Since(s.lastAt) > 2*time.Minute {
				s.dst.Close()
				delete(r.sessions, k)
			}
		}
		r.mu.Unlock()
	}
}

func (r *udpRelay) close() {
	r.once.Do(func() {
		close(r.closed)
		r.conn.Close()
	})
}

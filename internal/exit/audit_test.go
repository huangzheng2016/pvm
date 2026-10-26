package exit

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/huangzheng2016/pvm/internal/tunnel"
	"golang.zx2c4.com/wireguard/device"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEnsureCertDoesNotReplaceDamagedIdentity(t *testing.T) {
	for _, damage := range []string{"missing-key", "missing-cert", "bad-key", "bad-cert"} {
		t.Run(damage, func(t *testing.T) {
			dir := t.TempDir()
			if _, _, err := EnsureCert(dir); err != nil {
				t.Fatal(err)
			}
			certPath, keyPath := filepath.Join(dir, "server-cert.pem"), filepath.Join(dir, "server-key.pem")
			switch damage {
			case "missing-key":
				if err := os.Remove(keyPath); err != nil {
					t.Fatal(err)
				}
			case "missing-cert":
				if err := os.Remove(certPath); err != nil {
					t.Fatal(err)
				}
			case "bad-key":
				if err := os.WriteFile(keyPath, []byte("bad-key"), 0600); err != nil {
					t.Fatal(err)
				}
			case "bad-cert":
				if err := os.WriteFile(certPath, []byte("bad-cert"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			certBefore, _ := os.ReadFile(certPath)
			keyBefore, _ := os.ReadFile(keyPath)
			if _, _, err := EnsureCert(dir); err == nil {
				t.Fatal("damaged identity must require recovery")
			}
			certAfter, _ := os.ReadFile(certPath)
			keyAfter, _ := os.ReadFile(keyPath)
			if !bytes.Equal(certBefore, certAfter) || !bytes.Equal(keyBefore, keyAfter) {
				t.Fatal("existing identity was changed")
			}
		})
	}
}

func TestWGKeyReadErrorDoesNotGenerateKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wg.key")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateWGKey(path); err == nil {
		t.Fatal("want read error")
	} else {
		var pathErr *os.PathError
		if !errors.As(err, &pathErr) || pathErr.Op != "read" {
			t.Fatalf("expected original read error, got %v", err)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("unexpected generated files: %v", entries)
	}
}

func TestPeerPersistenceFailureRollsBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "peers.json")
	s, err := LoadPeerStore(path)
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := s.Allocate("key-a", "dev1", "udp")
	if err != nil {
		t.Fatal(err)
	}
	original := s.path
	s.path = filepath.Join(dir, "missing", "peers.json")
	if _, _, err := s.Allocate("key-b", "dev1", "udp"); err == nil {
		t.Fatal("want save error")
	}
	if got := s.List(); len(got) != 1 || got[0] != first {
		t.Fatalf("allocation changed store: %v", got)
	}
	if _, _, err := s.Remove(first.IP); err == nil {
		t.Fatal("want remove save error")
	}
	if got := s.List(); len(got) != 1 || got[0] != first {
		t.Fatalf("removal changed store: %v", got)
	}
	s.path = original
	reloaded, err := LoadPeerStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.List(); len(got) != 1 || got[0].IP != first.IP {
		t.Fatalf("disk changed: %v", got)
	}
}

func TestIptablesRemovalRequiresExactOwnership(t *testing.T) {
	save := `*filter
-A FORWARD -i pvmexit00 -m comment --comment "pvm-exit" -j ACCEPT
-A FORWARD -i pvmexit0 -m comment --comment "pvm-exit-other" -j ACCEPT
-A FORWARD -i pvmexit0 -m comment --comment "pvm-exit other" -j ACCEPT
-A FORWARD -i pvmexit0 -m comment --comment "pvm-exit" -j ACCEPT
COMMIT
`
	got := parseMarkedDeletes(save, "pvm-exit", []string{"pvmexit0"}, nil)
	if len(got) != 1 || !strings.Contains(strings.Join(got[0], " "), "--comment pvm-exit -j") {
		t.Fatalf("wrong rule ownership: %v", got)
	}
	if nftTableFor([]string{"dev10"}) == nftTableFor([]string{"dev20"}) {
		t.Fatal("numeric prefixes share nft table")
	}
}

func TestInvalidTunPrefixRejectedBeforeSideEffects(t *testing.T) {
	for _, prefix := range []string{"dev;delete table inet other", "dev-", "123", strings.Repeat("a", 15)} {
		if _, err := NewServer(ServerConfig{Token: "YOUR_TOKEN", TunPrefix: prefix, StateDir: t.TempDir()}); err == nil {
			t.Fatalf("accepted %q", prefix)
		}
	}
}

func tcpPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	a, err := net.DialTCP("tcp", nil, ln.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	b, err := ln.AcceptTCP()
	if err != nil {
		a.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(); b.Close() })
	_ = a.SetDeadline(time.Now().Add(5 * time.Second))
	_ = b.SetDeadline(time.Now().Add(5 * time.Second))
	return a, b
}

func TestIdleTimeoutRelayPreservesHalfClose(t *testing.T) {
	client, relayIn := tcpPair(t)
	relayOut, target := tcpPair(t)
	done := make(chan struct{})
	go func() {
		proxyTCP(&idleTimeoutConn{Conn: relayIn, d: time.Second}, &idleTimeoutConn{Conn: relayOut, d: time.Second})
		close(done)
	}()
	if _, err := io.WriteString(client, "request"); err != nil {
		t.Fatal(err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(target)
	if err != nil || string(got) != "request" {
		t.Fatalf("target EOF: %q %v", got, err)
	}
	if _, err := io.WriteString(target, "response"); err != nil {
		t.Fatal(err)
	}
	_ = target.CloseWrite()
	got, err = io.ReadAll(client)
	if err != nil || string(got) != "response" {
		t.Fatalf("client response: %q %v", got, err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("relay leaked")
	}
}

func TestDCShutdownClosesActiveConnections(t *testing.T) {
	client, server := tcpPair(t)
	dc := newDCServer(nil)
	done := make(chan struct{})
	go func() { dc.handle(server); close(done) }()
	// A partial request leaves the handler blocked awaiting the domain.
	if _, err := client.Write([]byte{dcVersion, dcCmdResolve, 0, 10}); err != nil {
		t.Fatal(err)
	}
	dc.close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("DC handler survived close")
	}
	dc.mu.Lock()
	defer dc.mu.Unlock()
	if len(dc.conns) != 0 || len(dc.active) != 0 {
		t.Fatal("DC retained peer bookkeeping")
	}
}

func TestUserspaceCleanupClosesHostTCP(t *testing.T) {
	u := newUserspaceBackend()
	conn, peer := tcpPair(t)
	started := make(chan struct{})
	u.serveTCP(conn, func() { close(started); _, _ = io.Copy(io.Discard, conn) })
	<-started
	if err := u.cleanup(nil, nil); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := peer.Read(b[:]); err == nil {
		t.Fatal("host TCP connection survived cleanup")
	}
}

func TestClosedSessionTablesRejectNewFlows(t *testing.T) {
	udp := newUDPSessionTable(time.Minute)
	udp.closeAll()
	if udp.putIfAbsent(udpFlow{}, &udpSession{}) {
		t.Fatal("UDP session accepted after close")
	}
	icmp := newICMPSessionTable(time.Minute)
	icmp.closeAll()
	if icmp.putIfAbsent(icmpFlow{}, &icmpSession{}) {
		t.Fatal("ICMP session accepted after close")
	}
}

var mockHTTPIP = netip.MustParseAddr("203.0.113.1")

func TestTunnelHTTPReadsRespectCancellation(t *testing.T) {
	srv := startUserspaceServer(t, "udp")
	u := srv.nat.(*userspaceBackend)
	entered := make(chan struct{}, 2)
	u.Intercept("203.0.113.1:8080", func(c net.Conn) { defer c.Close(); entered <- struct{}{}; _, _ = io.Copy(io.Discard, c) })
	entry := enrollViaTOFU(t, srv, "udp")
	tun, err := DialEntry(entry)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()
	ctx, cancel := context.WithTimeout(t.Context(), testOpTimeout)
	defer cancel()
	if err := tun.WaitHandshake(ctx, "10.99.0.1"); err != nil {
		t.Fatal(err)
	}
	for _, download := range []bool{false, true} {
		t.Run(fmt.Sprint(download), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				if download {
					_, _, err := tun.download(ctx, mockHTTPIP, 8080, "/")
					done <- err
				} else {
					_, err := tun.httpGet(ctx, mockHTTPIP, 8080, "example.com", "/")
					done <- err
				}
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("no TCP connection")
			}
			cancel()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("want cancellation error")
				}
			case <-time.After(time.Second):
				tun.Close()
				t.Fatal("HTTP read ignored cancellation")
			}
		})
	}
}

func TestHandshakeTimeoutDoesNotExposePrivateKey(t *testing.T) {
	srv, _ := inProcessServer(t, "udp")
	entry := enrollViaTOFU(t, srv, "udp")
	if err := NewClient(entry.Server, entry.Token, entry.Fingerprint).DeletePeer(t.Context(), entry.TunnelIP); err != nil {
		t.Fatal(err)
	}
	tun, err := DialEntry(entry)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()
	old := handshakeTimeout
	handshakeTimeout = time.Millisecond
	defer func() { handshakeTimeout = old }()
	err = tun.WaitHandshake(t.Context(), "10.99.0.1")
	secret, _ := tunnel.Hex(entry.WGPrivateKey)
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "private_key=") {
		t.Fatal("handshake timeout includes secret IPC or succeeded unexpectedly")
	}
}

func BenchmarkReapFreshPeers(b *testing.B) {
	dev := device.NewDevice(newChanTUN("dev1", 1420), tunnel.BindFor("udp"), device.NewLogger(device.LogLevelSilent, ""))
	defer dev.Close()
	s := &Server{store: &PeerStore{}, tr: map[string]*transportRT{"udp": {dev: dev}}}
	for i := 0; i < 512; i++ {
		key := make([]byte, 32)
		key[0] = byte(i)
		key[1] = byte(i >> 8)
		pubHex := hex.EncodeToString(key)
		if err := dev.IpcSet(fmt.Sprintf("public_key=%s\nallowed_ip=10.99.%d.%d/32\n", pubHex, i/254, i%254+2)); err != nil {
			b.Fatal(err)
		}
		pub := base64.StdEncoding.EncodeToString(key)
		s.store.Peers = append(s.store.Peers, Peer{IP: fmt.Sprintf("10.99.%d.%d", i/254, i%254+2), PubKey: pub, Transport: "udp", CreatedAt: time.Now()})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.reapIdleOnce()
	}
}

func TestServerCloseReleasesDomainListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dc := newDCServer(nil)
	done := make(chan struct{})
	go func() { dc.serve(ln); close(done) }()
	s := &Server{closed: make(chan struct{}), nat: newUserspaceBackend(), dc: dc, dcListeners: []net.Listener{ln}}
	s.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		ln.Close()
		t.Fatal("listener survived Server.Close")
	}
}

func TestTCPRelaySessionLimit(t *testing.T) {
	u := newUserspaceBackend()
	defer u.cleanup(nil, nil)
	// Fill the registry without creating thousands of sockets.
	for i := 0; i < tcpMaxSessions; i++ {
		a, b := net.Pipe()
		a.Close()
		b.Close()
		u.tcpConns[a] = struct{}{}
	}
	a, b := net.Pipe()
	defer b.Close()
	if u.serveTCP(a, func() { t.Error("handler ran above session limit") }) {
		t.Fatal("accepted flow above session limit")
	}
	var buf [1]byte
	if _, err := b.Read(buf[:]); err == nil {
		t.Fatal("rejected connection remains open")
	}
}

func TestSpeedConnPingHasCorrectContentLength(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	go serveSpeedConn(server)
	_ = client.SetDeadline(time.Now().Add(time.Second))
	if _, err := io.WriteString(client, "GET /ping HTTP/1.1\r\nHost: example.com\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"ok":true`) || resp.ContentLength != int64(len(body)) {
		t.Fatalf("bad ping framing: length=%d body=%q", resp.ContentLength, body)
	}
}

func TestSpeedConnHeadWritesHeaders(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	go serveSpeedConn(server)
	_ = client.SetDeadline(time.Now().Add(time.Second))
	req, _ := http.NewRequest(http.MethodHead, "http://example.com/speed?bytes=4096", nil)
	if err := req.Write(client); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(client), req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength != 4096 {
		t.Fatalf("bad HEAD headers: status=%d length=%d", resp.StatusCode, resp.ContentLength)
	}
}

func TestExitStateDirectoryLockLifecycle(t *testing.T) {
	oldLookup, oldSystem := ipinfoLookup, systemTZFn
	ipinfoLookup = func(context.Context) (string, error) { return "", fmt.Errorf("offline") }
	systemTZFn = func() string { return "" }
	defer func() { ipinfoLookup = oldLookup; systemTZFn = oldSystem }()
	cfg := ServerConfig{Token: "YOUR_TOKEN", StateDir: t.TempDir(), NAT: NATUserspace}
	const count = 8
	start := make(chan struct{})
	results := make(chan *Server, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; s, _ := NewServer(cfg); results <- s }()
	}
	close(start)
	wg.Wait()
	close(results)
	var first *Server
	for s := range results {
		if s != nil {
			if first != nil {
				s.Close()
				first.Close()
				t.Fatal("multiple servers acquired one state directory")
			}
			first = s
		}
	}
	if first == nil {
		t.Fatal("no server acquired state directory")
	}
	defer first.Close()
	first.Close()
	next, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("state directory remained locked after Close: %v", err)
	}
	next.Close()
}

func TestExitConstructionFailureReleasesStateLock(t *testing.T) {
	oldLookup, oldSystem := ipinfoLookup, systemTZFn
	ipinfoLookup = func(context.Context) (string, error) { return "", fmt.Errorf("offline") }
	systemTZFn = func() string { return "" }
	defer func() { ipinfoLookup = oldLookup; systemTZFn = oldSystem }()
	cfg := ServerConfig{Token: "YOUR_TOKEN", StateDir: t.TempDir(), NAT: NATUserspace, SubnetBase: 999}
	if s, err := NewServer(cfg); err == nil {
		s.Close()
		t.Fatal("expected invalid subnet error")
	}
	cfg.SubnetBase = 0
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("failed constructor retained lock: %v", err)
	}
	s.Close()
}

func TestClosedExitDoesNotMutateState(t *testing.T) {
	srv, _ := inProcessServer(t, "udp")
	entry := enrollViaTOFU(t, srv, "udp")
	before, err := os.ReadFile(filepath.Join(srv.cfg.StateDir, "peers.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv.Close()
	del := httptest.NewRequest(http.MethodDelete, "/v1/peers/"+entry.TunnelIP, nil)
	del.SetPathValue("ip", entry.TunnelIP)
	recorder := httptest.NewRecorder()
	srv.handleDeletePeer(recorder, del)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("delete after close: %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	srv.handleEnroll(recorder, httptest.NewRequest(http.MethodPost, "/v1/enroll", strings.NewReader(`{}`)))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("enroll after close: %d", recorder.Code)
	}
	srv.reapIdleOnce()
	after, err := os.ReadFile(filepath.Join(srv.cfg.StateDir, "peers.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("closed server changed peers.json")
	}
	if _, err := srv.Start(); err == nil {
		t.Fatal("closed server restarted without state lock")
	}
}

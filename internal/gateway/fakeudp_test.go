package gateway

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huangzheng2016/pvm/internal/gw/dns"
	gwpolicy "github.com/huangzheng2016/pvm/internal/gw/policy"
	"github.com/huangzheng2016/pvm/internal/gw/rules"
)

func newFakeUDPTestPolicy(t *testing.T) (*gwpolicy.Engine, *dns.Pool) {
	t.Helper()
	eng, err := rules.NewEngine("", nil)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := dns.NewPool("198.18.0.0/16", 0)
	if err != nil {
		t.Fatal(err)
	}
	return gwpolicy.New(eng, pool), pool
}

func udpEcho(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, src, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			conn.WriteToUDP(buf[:n], src)
		}
	}()
	return conn
}

func TestFakeUDPRelayRoundTrip(t *testing.T) {
	echo := udpEcho(t)
	eng, pool := newFakeUDPTestPolicy(t)
	fake := pool.Allocate("example.com")
	p := newFakeUDP(eng, "10.0.2.3", func(net.IP) string { return "dev1" })
	p.lookupDirect = func(context.Context, string) (net.IP, error) {
		return net.ParseIP("127.0.0.1"), nil
	}
	ln, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	p.ln = ln
	t.Cleanup(p.Close)
	client, err := net.DialUDP("udp4", nil, ln.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	src := client.LocalAddr().(*net.UDPAddr)
	dst := &net.UDPAddr{IP: fake, Port: echo.LocalAddr().(*net.UDPAddr).Port}
	key := fakeUDPKey{srcIP: src.IP.String(), srcPort: src.Port, dstIP: fake.String(), dstPort: dst.Port}
	if _, start := p.reserve(key); !start {
		t.Fatal("flow was not reserved")
	}
	go p.openFlow(key, src, dst, "example.com", []byte("ping"))
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 16)
	n, err := client.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "ping" {
		t.Fatalf("reply = %q, want ping", buf[:n])
	}
}

func TestFakeUDPProxyFailsClosed(t *testing.T) {
	eng, pool := newFakeUDPTestPolicy(t)
	eng.SetDefaultExit("my-exit")
	fake := pool.Allocate("example.com")
	p := newFakeUDP(eng, "10.0.2.3", nil)
	called := false
	p.lookupProxy = func(context.Context, string, string, int) (net.IP, error) {
		called = true
		return nil, nil
	}
	dst := &net.UDPAddr{IP: fake, Port: 443}
	key := fakeUDPKey{srcIP: "192.168.1.10", srcPort: 12345, dstIP: fake.String(), dstPort: 443}
	if _, start := p.reserve(key); !start {
		t.Fatal("flow was not reserved")
	}
	p.openFlow(key, &net.UDPAddr{IP: net.ParseIP("192.168.1.10"), Port: 12345}, dst, "example.com", []byte("x"))
	if called {
		t.Fatal("resolver must not run when the tunnel is down")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.flows) != 0 || len(p.pending) != 0 {
		t.Fatalf("failed flow was retained: flows=%d pending=%d", len(p.flows), len(p.pending))
	}
}

func TestFakeUDPSetPolicyClosesFlippedFlow(t *testing.T) {
	eng, pool := newFakeUDPTestPolicy(t)
	fake := pool.Allocate("example.com")
	p := newFakeUDP(eng, "10.0.2.3", nil)
	echo := udpEcho(t)
	up, err := net.DialUDP("udp4", nil, echo.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	key := fakeUDPKey{srcIP: "192.168.1.10", srcPort: 12345, dstIP: fake.String(), dstPort: 443}
	p.flows[key] = &fakeUDPFlow{key: key, domain: "example.com", vm: "dev1", route: gwpolicy.Direct, up: up}
	eng.SetVMExit("dev1", "my-exit")
	p.SetPolicy(eng)
	p.mu.Lock()
	if len(p.flows) != 0 {
		p.mu.Unlock()
		t.Fatal("flipped flow was retained")
	}
	p.mu.Unlock()
	_ = up.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := up.Read(make([]byte, 1)); err == nil {
		t.Fatal("flipped flow socket was not closed")
	}
	p.Close()
}

func TestFakeUDPProxyRelayLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("SO_MARK is Linux-specific")
	}
	echo := udpEcho(t)
	eng, pool := newFakeUDPTestPolicy(t)
	eng.SetDefaultExit("my-exit")
	fake := pool.Allocate("example.com")
	p := newFakeUDP(eng, "10.0.2.3", func(net.IP) string { return "dev1" })
	p.markFor = func(string) int { return 0 }
	p.lookupProxy = func(context.Context, string, string, int) (net.IP, error) {
		return net.ParseIP("127.0.0.1"), nil
	}
	ln, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	p.ln = ln
	t.Cleanup(p.Close)
	client, err := net.DialUDP("udp4", nil, ln.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	src := client.LocalAddr().(*net.UDPAddr)
	dst := &net.UDPAddr{IP: fake, Port: echo.LocalAddr().(*net.UDPAddr).Port}
	key := fakeUDPKey{srcIP: src.IP.String(), srcPort: src.Port, dstIP: fake.String(), dstPort: dst.Port}
	if _, start := p.reserve(key); !start {
		t.Fatal("flow was not reserved")
	}
	go p.openFlow(key, src, dst, "example.com", []byte("proxied"))
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 16)
	n, err := client.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "proxied" {
		t.Fatalf("reply = %q, want proxied", buf[:n])
	}
}

func TestProbeAddress(t *testing.T) {
	addrFor := func(string) (string, bool) { return "203.0.113.1:18080", true }
	ip, err := probeAddress(probeDomain, "my-exit", addrFor)
	if err != nil || ip.String() != "203.0.113.1" {
		t.Fatalf("probeAddress = %v, %v", ip, err)
	}
	ip, err = probeAddress("example.com", "my-exit", addrFor)
	if ip != nil || err != nil {
		t.Fatalf("non-probe domain = %v, %v", ip, err)
	}
	if _, err := probeAddress(probeDomain, "my-exit", func(string) (string, bool) { return "", false }); err == nil {
		t.Fatal("missing self-hosted probe must fail")
	}
}

func TestOriginalDstUDPLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux netfilter socket option")
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := enableUDPOriginalDst(conn); err != nil {
		t.Fatal(err)
	}
	addr := conn.LocalAddr().(*net.UDPAddr)
	if _, err := conn.WriteToUDP([]byte{1}, addr); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	oob := make([]byte, 128)
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, oobn, _, _, err := conn.ReadMsgUDP(buf, oob)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := originalDstUDP(oob[:oobn])
	if err != nil {
		t.Fatal(err)
	}
	if !dst.IP.Equal(addr.IP) || dst.Port != addr.Port {
		t.Fatalf("original destination = %v, want %v", dst, addr)
	}
}

// TestFakeUDPTransparentRelayLinux is an end-to-end regression gate for the
// tproxy + IP_TRANSPARENT fake-ip UDP relay. It builds an isolated topology
// inside two throwaway network namespaces whose names carry the test PID:
//
//	ns B (client 10.23.0.2) -- veth -- ns A (gateway 10.23.0.1)
//
// The child process in ns A installs the real fwmark policy route and
// nftables ruleset, runs the real fakeUDP listener via Start, and relays to
// a loopback echo server. A CONNECTED UDP client in ns B must see the echo
// come back from the allocated fake IP, not from the gateway address. The
// test never touches the host's own namespaces, bridges, or nft state and
// generates no third-party traffic.
func TestFakeUDPTransparentRelayLinux(t *testing.T) {
	switch os.Getenv(fakeUDPE2ERoleEnv) {
	case fakeUDPE2ERoleReceiver:
		fakeUDPTransparentReceiver(t)
		return
	case fakeUDPE2ERoleSender:
		fakeUDPTransparentSender(t)
		return
	}
	if runtime.GOOS != "linux" {
		t.Skip("tproxy and network namespaces are Linux-specific")
	}
	if os.Geteuid() != 0 {
		t.Skip("creating network namespaces requires root")
	}
	for _, tool := range []string{"ip", "nft"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}

	pid := os.Getpid()
	nsA := fmt.Sprintf("pvmfu%da", pid)
	nsB := fmt.Sprintf("pvmfu%db", pid)
	ifA := fmt.Sprintf("fua%d", pid)
	ifB := fmt.Sprintf("fub%d", pid)

	runIP := func(args ...string) {
		t.Helper()
		out, err := exec.Command("ip", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("ip %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	// Registered before the helpers so it runs after them (cleanups are
	// LIFO): deleting a namespace destroys the veth ends inside it, and the
	// receiver must already be dead or its namespace would outlive the name.
	t.Cleanup(func() {
		exec.Command("ip", "netns", "del", nsA).Run()
		exec.Command("ip", "netns", "del", nsB).Run()
		// only reachable when creation failed midway, leaving an end in
		// the root namespace
		exec.Command("ip", "link", "del", ifA).Run()
		exec.Command("ip", "link", "del", ifB).Run()
	})

	runIP("netns", "add", nsA)
	runIP("netns", "add", nsB)
	runIP("link", "add", ifA, "type", "veth", "peer", "name", ifB)
	runIP("link", "set", ifA, "netns", nsA)
	runIP("link", "set", ifB, "netns", nsB)
	runIP("-n", nsA, "addr", "add", "10.23.0.1/24", "dev", ifA)
	runIP("-n", nsA, "link", "set", "lo", "up")
	runIP("-n", nsA, "link", "set", ifA, "up")
	runIP("-n", nsB, "addr", "add", "10.23.0.2/24", "dev", ifB)
	runIP("-n", nsB, "link", "set", "lo", "up")
	runIP("-n", nsB, "link", "set", ifB, "up")
	runIP("-n", nsB, "route", "replace", "default", "via", "10.23.0.1")

	recvCtx, recvCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer recvCancel()
	recv := exec.CommandContext(recvCtx, "ip", "netns", "exec", nsA, self, fakeUDPE2ERunFlag)
	recv.WaitDelay = 10 * time.Second
	recv.Env = append(os.Environ(),
		fakeUDPE2ERoleEnv+"="+fakeUDPE2ERoleReceiver,
		fakeUDPE2EIfEnv+"="+ifA,
	)
	stdin, err := recv.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := recv.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	recvStdout := &boundedBuffer{}
	recvStderr := &boundedBuffer{}
	recv.Stderr = recvStderr
	if err := recv.Start(); err != nil {
		t.Fatalf("start receiver: %v", err)
	}
	t.Cleanup(func() {
		recvCancel()
		if recv.Process != nil {
			_ = recv.Process.Kill()
		}
	})
	waitCh := make(chan error, 1)
	go func() { waitCh <- recv.Wait() }()

	type readyInfo struct {
		ip   string
		port int
	}
	readyCh := make(chan readyInfo, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := sc.Text()
			fmt.Fprintln(recvStdout, line)
			rest, ok := strings.CutPrefix(line, "ready ")
			if !ok {
				continue
			}
			fields := strings.Fields(rest)
			if len(fields) != 2 {
				continue
			}
			port, err := strconv.Atoi(fields[1])
			if err != nil {
				continue
			}
			select {
			case readyCh <- readyInfo{ip: fields[0], port: port}:
			default:
			}
		}
	}()

	var ready readyInfo
	select {
	case ready = <-readyCh:
	case err := <-waitCh:
		t.Fatalf("receiver exited before ready: %v\nstdout:\n%s\nstderr:\n%s", err, recvStdout, recvStderr)
	case <-time.After(20 * time.Second):
		t.Fatalf("timed out waiting for receiver ready\nstdout:\n%s\nstderr:\n%s", recvStdout, recvStderr)
	}

	senderCtx, senderCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer senderCancel()
	senderOut := &boundedBuffer{}
	sender := exec.CommandContext(senderCtx, "ip", "netns", "exec", nsB, self, fakeUDPE2ERunFlag)
	sender.WaitDelay = 10 * time.Second
	sender.Env = append(os.Environ(),
		fakeUDPE2ERoleEnv+"="+fakeUDPE2ERoleSender,
		fakeUDPE2EFakeIPEnv+"="+ready.ip,
		fakeUDPE2EPortEnv+"="+strconv.Itoa(ready.port),
	)
	sender.Stdout = senderOut
	sender.Stderr = senderOut
	if err := sender.Run(); err != nil {
		t.Fatalf("sender: %v\n%s", err, senderOut)
	}

	if err := stdin.Close(); err != nil {
		t.Fatalf("close receiver stdin: %v", err)
	}
	select {
	case err := <-waitCh:
		if err != nil {
			t.Fatalf("receiver failed: %v\nstdout:\n%s\nstderr:\n%s", err, recvStdout, recvStderr)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("receiver did not exit after stdin close\nstdout:\n%s\nstderr:\n%s", recvStdout, recvStderr)
	}
}

const (
	fakeUDPE2ERoleEnv   = "PVM_FAKEUDP_E2E_ROLE"
	fakeUDPE2EIfEnv     = "PVM_FAKEUDP_E2E_IFACE"
	fakeUDPE2EFakeIPEnv = "PVM_FAKEUDP_E2E_FAKE_IP"
	fakeUDPE2EPortEnv   = "PVM_FAKEUDP_E2E_PORT"

	fakeUDPE2ERoleReceiver = "receiver"
	fakeUDPE2ERoleSender   = "sender"

	fakeUDPE2ERunFlag = "-test.run=^TestFakeUDPTransparentRelayLinux$"

	// fakeUDPE2ENoUplink names an interface that never exists in the test
	// namespace: RenderNFT's uplink only drives masquerade and forward
	// rules, which must never fire here. Masquerading the relay's raw reply
	// would rewrite its source to the gateway IP and defeat the point of
	// the test.
	fakeUDPE2ENoUplink = "pvmfu0x"
)

// boundedBuffer keeps the tail of helper output so failure diagnostics stay
// readable without letting a chatty child grow memory without bound.
type boundedBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if len(b.data) > 8192 {
		b.data = append([]byte(nil), b.data[len(b.data)-8192:]...)
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

// fakeUDPTransparentReceiver runs inside namespace A. It installs the real
// fake-ip policy route and nftables ruleset, then relays UDP for the
// allocated fake IP to a loopback echo server. The single "ready" line on
// stdout is the handshake with the parent; the relay lives until stdin EOF.
func fakeUDPTransparentReceiver(t *testing.T) {
	t.Helper()
	if err := ensureFakeIPRoute(); err != nil {
		t.Fatalf("ensureFakeIPRoute: %v", err)
	}
	iface := os.Getenv(fakeUDPE2EIfEnv)
	if iface == "" {
		t.Fatalf("%s is not set", fakeUDPE2EIfEnv)
	}
	cfg := &Config{CIDR: "10.23.0.0/24", GwIP: "10.23.0.1"}
	if err := ApplyNFT(RenderNFT(cfg, fakeUDPE2ENoUplink, iface, nil)); err != nil {
		t.Fatalf("ApplyNFT: %v", err)
	}
	echo := udpEcho(t)
	echoPort := echo.LocalAddr().(*net.UDPAddr).Port
	eng, pool := newFakeUDPTestPolicy(t)
	fake := pool.Allocate("example.com")
	p := newFakeUDP(eng, "10.23.0.1", func(net.IP) string { return "dev1" })
	p.lookupDirect = func(context.Context, string) (net.IP, error) {
		return net.ParseIP("127.0.0.1"), nil
	}
	if err := p.Start("10.23.0.1", fakeUDPPort); err != nil {
		t.Fatalf("fakeudp start: %v", err)
	}
	t.Cleanup(p.Close)
	fmt.Printf("ready %s %d\n", fake.String(), echoPort)
	// The parent closes stdin once the sender finished; returning lets the
	// cleanup close the relay.
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Error("timed out waiting for stdin EOF")
	}
}

// fakeUDPTransparentSender runs inside namespace B and exercises the path a
// VM guest would take: a CONNECTED UDP socket to the fake IP. The kernel
// only delivers datagrams whose source matches the connected peer, so the
// read succeeds if and only if the relay answered from fakeIP:port; a reply
// from the gateway address is dropped and the deadline below fires instead.
func fakeUDPTransparentSender(t *testing.T) {
	t.Helper()
	ip := net.ParseIP(os.Getenv(fakeUDPE2EFakeIPEnv))
	port, err := strconv.Atoi(os.Getenv(fakeUDPE2EPortEnv))
	if ip == nil || err != nil || port <= 0 {
		t.Fatalf("bad fake address %q port %q", os.Getenv(fakeUDPE2EFakeIPEnv), os.Getenv(fakeUDPE2EPortEnv))
	}
	conn, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: ip, Port: port})
	if err != nil {
		t.Fatalf("DialUDP: %v", err)
	}
	defer conn.Close()
	payload := []byte("pvm-fakeudp-transparent-e2e")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 128)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if !bytes.Equal(buf[:n], payload) {
		t.Fatalf("reply = %q, want %q", buf[:n], payload)
	}
}

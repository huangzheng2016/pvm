package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRealIPForwardE2ELinux is an end-to-end gate for REAL-IP L3 forwarding:
// UDP, ICMP, and a bulk TCP transfer cross the gateway as ordinary forwarded
// traffic (policy route + masquerade), with no fake-ip relay involved. It
// builds three throwable network namespaces whose names carry the test PID:
//
//	ns B (client 10.23.0.2) -- veth -- ns A (gateway 10.23.0.1)
//	ns A (pvmwg0 10.100.0.7) -- veth -- ns C (exit 10.100.0.1, 192.0.2.10 on lo)
//
// pvmwg0 stands in for the WireGuard tunnel interface (the exit package tests
// already cover the real WireGuard data plane); the name must match
// tunnelIfName(0) because RenderNFT renders the tunnel masquerade set from
// it. The gateway child installs the real RenderNFT ruleset plus the
// production-style policy route (table 100 + source rule). 192.0.2.10 is
// TEST-NET-1 documentation space and never leaves the namespaces. The host's
// own namespaces, bridges, and nft state are never touched and no third-party
// traffic is generated.
func TestRealIPForwardE2ELinux(t *testing.T) {
	switch os.Getenv(realipE2ERoleEnv) {
	case realipE2ERoleGateway:
		realipE2EGateway(t)
		return
	case realipE2ERoleExit:
		realipE2EExit(t)
		return
	case realipE2ERoleClient:
		realipE2EClient(t)
		return
	}
	if runtime.GOOS != "linux" {
		t.Skip("network namespaces and nftables are Linux-specific")
	}
	if os.Geteuid() != 0 {
		t.Skip("creating network namespaces requires root")
	}
	for _, tool := range []string{"ip", "nft", "ping"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}

	pid := os.Getpid()
	nsA := fmt.Sprintf("pvmri%da", pid)
	nsB := fmt.Sprintf("pvmri%db", pid)
	nsC := fmt.Sprintf("pvmri%dc", pid)
	ifA := fmt.Sprintf("ria%d", pid)
	ifB := fmt.Sprintf("rib%d", pid)
	ifC := fmt.Sprintf("ric%d", pid)
	tunTmp := fmt.Sprintf("rit%d", pid)

	runIP := func(args ...string) {
		t.Helper()
		out, err := exec.Command("ip", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("ip %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	// Registered before the helpers so it runs after them (cleanups are
	// LIFO): deleting a namespace destroys the veth ends inside it, and the
	// children must already be dead or their namespace would outlive the
	// name.
	t.Cleanup(func() {
		exec.Command("ip", "netns", "del", nsA).Run()
		exec.Command("ip", "netns", "del", nsB).Run()
		exec.Command("ip", "netns", "del", nsC).Run()
		exec.Command("ip", "link", "del", ifA).Run()
		exec.Command("ip", "link", "del", ifB).Run()
		exec.Command("ip", "link", "del", ifC).Run()
		exec.Command("ip", "link", "del", tunTmp).Run()
	})

	runIP("netns", "add", nsA)
	runIP("netns", "add", nsB)
	runIP("netns", "add", nsC)
	runIP("link", "add", ifA, "type", "veth", "peer", "name", ifB)
	runIP("link", "set", ifA, "netns", nsA)
	runIP("-n", nsA, "link", "set", ifA, "name", "br0")
	runIP("link", "set", ifB, "netns", nsB)
	runIP("link", "add", tunTmp, "type", "veth", "peer", "name", ifC)
	runIP("link", "set", tunTmp, "netns", nsA)
	runIP("-n", nsA, "link", "set", tunTmp, "name", realipE2ETunIf)
	runIP("link", "set", ifC, "netns", nsC)

	runIP("-n", nsA, "addr", "add", "10.23.0.1/24", "dev", "br0")
	runIP("-n", nsA, "link", "set", "lo", "up")
	runIP("-n", nsA, "link", "set", "br0", "up")
	runIP("-n", nsB, "addr", "add", "10.23.0.2/24", "dev", ifB)
	runIP("-n", nsB, "link", "set", "lo", "up")
	runIP("-n", nsB, "link", "set", ifB, "up")
	runIP("-n", nsB, "route", "replace", "default", "via", "10.23.0.1")

	// The tunnel link runs at the UDP transport MTU while the client link
	// stays at the default 1500, so the bulk TCP transfer exercises the
	// tunnel-MTU path.
	runIP("-n", nsA, "addr", "add", "10.100.0.7/32", "dev", realipE2ETunIf)
	runIP("-n", nsA, "link", "set", realipE2ETunIf, "up")
	runIP("-n", nsA, "link", "set", realipE2ETunIf, "mtu", "1420")
	runIP("-n", nsC, "addr", "add", "10.100.0.1/32", "dev", ifC)
	runIP("-n", nsC, "link", "set", ifC, "up")
	runIP("-n", nsC, "link", "set", ifC, "mtu", "1420")
	runIP("-n", nsC, "addr", "add", realipE2EExitIP+"/32", "dev", "lo")
	runIP("-n", nsC, "link", "set", "lo", "up")
	// /32 peering leaves no connected prefix, so the gateway host route is
	// forced onlink for the broadcast veth.
	runIP("-n", nsC, "route", "replace", "default", "via", "10.100.0.7", "dev", ifC, "onlink")

	gw := startRealipE2EChild(t, self, nsA, realipE2ERoleGateway, realipE2EIfEnv+"=br0")
	ex := startRealipE2EChild(t, self, nsC, realipE2ERoleExit)
	waitRealipE2EReady(t, "gateway", gw)
	waitRealipE2EReady(t, "exit", ex)

	clientCtx, clientCancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer clientCancel()
	clientOut := &realipE2ETailBuf{}
	client := exec.CommandContext(clientCtx, "ip", "netns", "exec", nsB, self, realipE2ERunFlag)
	client.WaitDelay = 10 * time.Second
	client.Env = append(os.Environ(), realipE2ERoleEnv+"="+realipE2ERoleClient)
	client.Stdout = clientOut
	client.Stderr = clientOut
	if err := client.Run(); err != nil {
		t.Fatalf("client: %v\n%s", err, clientOut)
	}

	for _, h := range []struct {
		name  string
		child *realipE2EChild
	}{{"gateway", gw}, {"exit", ex}} {
		if err := h.child.stdin.Close(); err != nil {
			t.Fatalf("close %s helper stdin: %v", h.name, err)
		}
	}
	for _, h := range []struct {
		name  string
		child *realipE2EChild
	}{{"gateway", gw}, {"exit", ex}} {
		select {
		case err := <-h.child.wait:
			if err != nil {
				t.Fatalf("%s helper failed: %v\nstdout:\n%s\nstderr:\n%s", h.name, err, h.child.stdout, h.child.stderr)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("%s helper did not exit after stdin close\nstdout:\n%s\nstderr:\n%s", h.name, h.child.stdout, h.child.stderr)
		}
	}
}

const (
	realipE2ERoleEnv = "PVM_REALIP_E2E_ROLE"
	realipE2EIfEnv   = "PVM_REALIP_E2E_IFACE"

	realipE2ERoleGateway = "gateway"
	realipE2ERoleExit    = "exit"
	realipE2ERoleClient  = "client"

	realipE2ERunFlag = "-test.run=^TestRealIPForwardE2ELinux$"

	// realipE2ETunIf must equal tunnelIfName(0): RenderNFT renders the exit
	// masquerade set from that name, and the policy route binds table 100
	// to the interface.
	realipE2ETunIf = "pvmwg0"
	// realipE2ENoUplink names an interface that never exists in the test
	// namespace: br->uplink forwarding is default-deny, so nothing may
	// escape the gateway except via the tunnel masquerade.
	realipE2ENoUplink = "pvm-no-uplink"

	// realipE2EExitIP is TEST-NET-1 documentation space; it exists only
	// inside the throwaway namespaces. realipE2EDeadIP is its nonresponsive
	// neighbor for the negative controls.
	realipE2EExitIP = "192.0.2.10"
	realipE2EDeadIP = "192.0.2.11"

	realipE2EUDPPort = 18082
	realipE2ETCPPort = 18080
)

// realipE2ETailBuf keeps the tail of helper output so failure diagnostics
// stay readable without letting a chatty child grow memory without bound. It
// deliberately mirrors fakeudp_test.go's boundedBuffer under a distinct name
// so this file does not depend on that helper surviving renames.
type realipE2ETailBuf struct {
	mu   sync.Mutex
	data []byte
}

func (b *realipE2ETailBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if len(b.data) > 8192 {
		b.data = append([]byte(nil), b.data[len(b.data)-8192:]...)
	}
	return len(p), nil
}

func (b *realipE2ETailBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

// realipE2EChild tracks one self-exec helper process plus its stdout
// handshake state.
type realipE2EChild struct {
	stdin  io.WriteCloser
	stdout *realipE2ETailBuf
	stderr *realipE2ETailBuf
	ready  chan string
	wait   chan error
}

func startRealipE2EChild(t *testing.T, self, ns, role string, env ...string) *realipE2EChild {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	cmd := exec.CommandContext(ctx, "ip", "netns", "exec", ns, self, realipE2ERunFlag)
	cmd.WaitDelay = 10 * time.Second
	cmd.Env = append(os.Environ(), append(env, realipE2ERoleEnv+"="+role)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	c := &realipE2EChild{
		stdin:  stdin,
		stdout: &realipE2ETailBuf{},
		stderr: &realipE2ETailBuf{},
		ready:  make(chan string, 1),
		wait:   make(chan error, 1),
	}
	cmd.Stderr = c.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s helper: %v", role, err)
	}
	t.Cleanup(func() {
		cancel()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	go func() { c.wait <- cmd.Wait() }()
	go func() {
		sc := bufio.NewScanner(stdoutPipe)
		for sc.Scan() {
			line := sc.Text()
			fmt.Fprintln(c.stdout, line)
			if rest, ok := strings.CutPrefix(line, "ready "); ok {
				select {
				case c.ready <- rest:
				default:
				}
			}
		}
	}()
	return c
}

func waitRealipE2EReady(t *testing.T, name string, c *realipE2EChild) {
	t.Helper()
	select {
	case <-c.ready:
	case err := <-c.wait:
		t.Fatalf("%s helper exited before ready: %v\nstdout:\n%s\nstderr:\n%s", name, err, c.stdout, c.stderr)
	case <-time.After(20 * time.Second):
		t.Fatalf("timed out waiting for %s helper ready\nstdout:\n%s\nstderr:\n%s", name, c.stdout, c.stderr)
	}
}

// realipE2EGateway runs inside namespace A and turns it into the production
// gateway data plane for the client subnet: forwarding with loose rp_filter,
// the table-100 policy route for 10.23.0.0/24, and the real RenderNFT
// ruleset masquerading tunnel-bound traffic. The single "ready" line on
// stdout is the handshake with the parent; the process lives until stdin EOF.
func realipE2EGateway(t *testing.T) {
	t.Helper()
	// /proc sysctls are per-network-namespace: these writes land in ns A only.
	for path, val := range map[string]string{
		"/proc/sys/net/ipv4/ip_forward":             "1",
		"/proc/sys/net/ipv4/conf/all/rp_filter":     "2",
		"/proc/sys/net/ipv4/conf/default/rp_filter": "2",
	} {
		if err := os.WriteFile(path, []byte(val), 0); err != nil {
			t.Fatalf("sysctl %s: %v", path, err)
		}
	}
	runIP := func(args ...string) {
		t.Helper()
		out, err := exec.Command("ip", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("ip %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	// The production exit policy route: tunnel default in its own table,
	// selected for the client subnet by source.
	runIP("route", "replace", "default", "dev", realipE2ETunIf, "table", "100")
	runIP("rule", "add", "from", "10.23.0.0/24", "pref", "100", "lookup", "100")
	// Loose rp_filter still requires a resolvable source: return traffic
	// from the exit address arrives on pvmwg0 and must resolve in the main
	// table or the kernel drops it before conntrack can un-NAT.
	runIP("route", "replace", realipE2EExitIP+"/32", "dev", realipE2ETunIf)

	iface := os.Getenv(realipE2EIfEnv)
	if iface == "" {
		t.Fatalf("%s is not set", realipE2EIfEnv)
	}
	cfg := &Config{
		CIDR:        "10.23.0.0/24",
		GwIP:        "10.23.0.1",
		DefaultExit: "my-exit",
		Exits:       map[string]ExitDef{"my-exit": {Transport: "udp", MTU: 1420}},
	}
	if err := ApplyNFT(RenderNFT(cfg, realipE2ENoUplink, iface, nil)); err != nil {
		t.Fatalf("ApplyNFT: %v", err)
	}
	a := &Agent{upl: realipE2ENoUplink}
	if err := a.checkNFT(cfg); err != nil {
		t.Fatalf("checkNFT: %v", err)
	}
	fmt.Println("ready gateway")
	realipE2EWaitStdin(t)
}

// realipE2EExit runs inside namespace C and plays the VPS exit: plain UDP and
// TCP echo servers on the TEST-NET-1 address. No fake-ip listener exists
// anywhere in this topology, so an echo can only come from the real
// destination. Lifetime is the same stdin handshake as the gateway helper.
func realipE2EExit(t *testing.T) {
	t.Helper()
	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(realipE2EExitIP), Port: realipE2EUDPPort})
	if err != nil {
		t.Fatalf("udp listen: %v", err)
	}
	t.Cleanup(func() { udpConn.Close() })
	tcpLn, err := net.Listen("tcp4", fmt.Sprintf("%s:%d", realipE2EExitIP, realipE2ETCPPort))
	if err != nil {
		t.Fatalf("tcp listen: %v", err)
	}
	t.Cleanup(func() { tcpLn.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, src, err := udpConn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if _, err := udpConn.WriteToUDP(buf[:n], src); err != nil {
				return
			}
		}
	}()
	go func() {
		for {
			c, err := tcpLn.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	fmt.Println("ready exit")
	realipE2EWaitStdin(t)
}

// realipE2EClient runs inside namespace B and exercises the guest path: a
// connected UDP socket, ping, and a bulk TCP transfer to the real exit
// address, plus negative controls against a nonresponsive TEST-NET-1
// neighbor. Connected sockets only accept traffic from the dialed peer, so
// an exact echo proves the reply came from 192.0.2.10 itself - no fake-ip is
// allocated anywhere in this topology and the destination is never rewritten.
func realipE2EClient(t *testing.T) {
	t.Helper()
	exitIP := net.ParseIP(realipE2EExitIP)

	payload := make([]byte, 512)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}
	udpConn, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: exitIP, Port: realipE2EUDPPort})
	if err != nil {
		t.Fatalf("DialUDP: %v", err)
	}
	defer udpConn.Close()
	if _, err := udpConn.Write(payload); err != nil {
		t.Fatalf("udp write: %v", err)
	}
	_ = udpConn.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 2048)
	n, err := udpConn.Read(buf)
	if err != nil {
		t.Fatalf("udp read reply: %v", err)
	}
	if !bytes.Equal(buf[:n], payload) {
		t.Fatalf("udp reply does not match payload (%d bytes)", n)
	}

	if out, err := exec.Command("ping", "-n", "-c", "2", "-W", "3", realipE2EExitIP).CombinedOutput(); err != nil {
		t.Fatalf("ping %s: %v\n%s", realipE2EExitIP, err, out)
	}

	// 128 KiB patterned transfer, well past the 64 KiB gate, echoed
	// byte-for-byte. Reads run concurrently with writes so neither
	// direction can deadlock on full socket buffers.
	const total = 128 << 10
	pattern := func(off int) byte { return byte(off*31 + 7) }
	tcpConn, err := net.DialTCP("tcp4", nil, &net.TCPAddr{IP: exitIP, Port: realipE2ETCPPort})
	if err != nil {
		t.Fatalf("DialTCP: %v", err)
	}
	defer tcpConn.Close()
	_ = tcpConn.SetDeadline(time.Now().Add(60 * time.Second))
	readErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 4096)
		for off := 0; off < total; {
			n, err := tcpConn.Read(buf)
			for i := 0; i < n; i++ {
				if buf[i] != pattern(off+i) {
					readErr <- fmt.Errorf("echo mismatch at offset %d", off+i)
					return
				}
			}
			off += n
			if err != nil {
				readErr <- err
				return
			}
		}
		readErr <- nil
	}()
	writeErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 4096)
		for off := 0; off < total; off += len(buf) {
			for i := range buf {
				buf[i] = pattern(off + i)
			}
			if _, err := tcpConn.Write(buf); err != nil {
				writeErr <- err
				return
			}
		}
		writeErr <- nil
	}()
	if err := <-writeErr; err != nil {
		t.Fatalf("tcp write: %v", err)
	}
	if err := <-readErr; err != nil {
		t.Fatalf("tcp read: %v", err)
	}

	deadConn, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.ParseIP(realipE2EDeadIP), Port: realipE2EUDPPort})
	if err != nil {
		t.Fatalf("DialUDP dead: %v", err)
	}
	defer deadConn.Close()
	if _, err := deadConn.Write([]byte("pvm-realip-e2e")); err != nil {
		t.Fatalf("dead udp write: %v", err)
	}
	_ = deadConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if n, err := deadConn.Read(buf); err == nil {
		t.Fatalf("unexpected udp reply from %s: %q", realipE2EDeadIP, buf[:n])
	}
	if out, err := exec.Command("ping", "-n", "-c", "1", "-W", "2", realipE2EDeadIP).CombinedOutput(); err == nil {
		t.Fatalf("ping %s unexpectedly succeeded:\n%s", realipE2EDeadIP, out)
	}
}

// realipE2EWaitStdin keeps a helper alive until the parent closes its stdin,
// bounded so a stuck parent cannot hang the helper forever.
func realipE2EWaitStdin(t *testing.T) {
	t.Helper()
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

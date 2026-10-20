package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	fakeICMPE2ERoleEnv   = "PVM_FAKEICMP_E2E_ROLE"
	fakeICMPE2EIfEnv     = "PVM_FAKEICMP_E2E_IFACE"
	fakeICMPE2EFakeIPEnv = "PVM_FAKEICMP_E2E_FAKE_IP"
	fakeICMPE2EIDEnv     = "PVM_FAKEICMP_E2E_ID"
	fakeICMPE2EWantEnv   = "PVM_FAKEICMP_E2E_WANT_REPLY"

	fakeICMPE2ERoleGateway = "gateway"
	fakeICMPE2ERoleClient  = "client"

	fakeICMPE2ERunFlag = "-test.run=^TestFakeICMPNetnsRelayLinux$"

	// fakeICMPE2ENoUplink names an interface that never exists in the test
	// namespace: RenderNFT's uplink only drives masquerade and forward
	// rules, which must never fire here. Masquerading the relay's raw
	// reply would rewrite its source to the gateway IP and defeat the
	// point of the test.
	fakeICMPE2ENoUplink = "pvmfi0x"

	fakeICMPE2EClusterCIDR = "10.23.0.0/24"
	fakeICMPE2ETargetIP    = "10.23.1.2"
)

// TestFakeICMPNetnsRelayLinux is an end-to-end regression gate for the
// fake-ip ICMP relay. It builds an isolated topology inside three
// throwaway network namespaces whose names carry the test PID:
//
//	ns client (10.23.0.2) -- veth -- ns gateway (10.23.0.1) -- veth -- ns target (10.23.1.2)
//
// The gateway child installs the real fake-ip policy route and the real
// nftables ruleset (the mark-only ICMP prerouting rule plus the output
// chain that drops the kernel's false fake-source echo replies), runs
// the real fakeICMP relay via Start, and relays to the target namespace.
// Two phases:
//
//   - answering target: a real ICMP echo client in the client namespace
//     pings the fake IP and must get the reply the target sent, proven
//     by the target's InEchos counter moving;
//   - silent target (net.ipv4.icmp_echo_ignore_all=1): the same ping
//     must get NOTHING — any reply would be a local fake success from
//     the gateway kernel, the exact bug this test gates.
//
// The test never touches the host's own namespaces, bridges, or nft
// state and generates no third-party traffic.
func TestFakeICMPNetnsRelayLinux(t *testing.T) {
	switch os.Getenv(fakeICMPE2ERoleEnv) {
	case fakeICMPE2ERoleGateway:
		fakeICMPNetnsGateway(t)
		return
	case fakeICMPE2ERoleClient:
		fakeICMPNetnsClient(t)
		return
	}
	if runtime.GOOS != "linux" {
		t.Skip("network namespaces and nftables are Linux-specific")
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
	nsGW := fmt.Sprintf("pvmfi%dg", pid)
	nsCL := fmt.Sprintf("pvmfi%dc", pid)
	nsTG := fmt.Sprintf("pvmfi%dt", pid)
	ifGW := fmt.Sprintf("fig%d", pid)
	ifCL := fmt.Sprintf("fic%d", pid)
	ifGT := fmt.Sprintf("fog%d", pid)
	ifTG := fmt.Sprintf("fot%d", pid)

	runIP := func(args ...string) {
		t.Helper()
		out, err := exec.Command("ip", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("ip %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	// Registered before the helpers so it runs after them (cleanups are
	// LIFO): deleting a namespace destroys the veth ends inside it, and
	// the gateway child must already be dead or its namespace would
	// outlive the name.
	t.Cleanup(func() {
		exec.Command("ip", "netns", "del", nsGW).Run()
		exec.Command("ip", "netns", "del", nsCL).Run()
		exec.Command("ip", "netns", "del", nsTG).Run()
		// only reachable when creation failed midway, leaving an end in
		// the root namespace
		exec.Command("ip", "link", "del", ifGW).Run()
		exec.Command("ip", "link", "del", ifCL).Run()
		exec.Command("ip", "link", "del", ifGT).Run()
		exec.Command("ip", "link", "del", ifTG).Run()
	})

	runIP("netns", "add", nsGW)
	runIP("netns", "add", nsCL)
	runIP("netns", "add", nsTG)
	runIP("link", "add", ifGW, "type", "veth", "peer", "name", ifCL)
	runIP("link", "set", ifGW, "netns", nsGW)
	runIP("link", "set", ifCL, "netns", nsCL)
	runIP("-n", nsGW, "addr", "add", "10.23.0.1/24", "dev", ifGW)
	runIP("-n", nsGW, "link", "set", "lo", "up")
	runIP("-n", nsGW, "link", "set", ifGW, "up")
	runIP("-n", nsCL, "addr", "add", "10.23.0.2/24", "dev", ifCL)
	runIP("-n", nsCL, "link", "set", "lo", "up")
	runIP("-n", nsCL, "link", "set", ifCL, "up")
	runIP("-n", nsCL, "route", "replace", "default", "via", "10.23.0.1")
	runIP("link", "add", ifGT, "type", "veth", "peer", "name", ifTG)
	runIP("link", "set", ifGT, "netns", nsGW)
	runIP("link", "set", ifTG, "netns", nsTG)
	runIP("-n", nsGW, "addr", "add", "10.23.1.1/24", "dev", ifGT)
	runIP("-n", nsGW, "link", "set", ifGT, "up")
	runIP("-n", nsTG, "addr", "add", "10.23.1.2/24", "dev", ifTG)
	runIP("-n", nsTG, "link", "set", "lo", "up")
	runIP("-n", nsTG, "link", "set", ifTG, "up")

	gwCtx, gwCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer gwCancel()
	gw := exec.CommandContext(gwCtx, "ip", "netns", "exec", nsGW, self, fakeICMPE2ERunFlag)
	gw.WaitDelay = 10 * time.Second
	gw.Env = append(os.Environ(),
		fakeICMPE2ERoleEnv+"="+fakeICMPE2ERoleGateway,
		fakeICMPE2EIfEnv+"="+ifGW,
	)
	stdin, err := gw.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := gw.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	gwStdout := &boundedBuffer{}
	gwStderr := &boundedBuffer{}
	gw.Stderr = gwStderr
	if err := gw.Start(); err != nil {
		t.Fatalf("start gateway: %v", err)
	}
	t.Cleanup(func() {
		gwCancel()
		if gw.Process != nil {
			_ = gw.Process.Kill()
		}
	})
	waitCh := make(chan error, 1)
	go func() { waitCh <- gw.Wait() }()

	readyCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := sc.Text()
			fmt.Fprintln(gwStdout, line)
			rest, ok := strings.CutPrefix(line, "ready ")
			if !ok {
				continue
			}
			select {
			case readyCh <- rest:
			default:
			}
		}
	}()

	var fakeIP string
	select {
	case fakeIP = <-readyCh:
	case err := <-waitCh:
		t.Fatalf("gateway exited before ready: %v\nstdout:\n%s\nstderr:\n%s", err, gwStdout, gwStderr)
	case <-time.After(20 * time.Second):
		t.Fatalf("timed out waiting for gateway ready\nstdout:\n%s\nstderr:\n%s", gwStdout, gwStderr)
	}

	// Phase 1: the target answers. The client must see the relayed reply,
	// and the target's InEchos counter must move, proving the request
	// really crossed the gateway and the reply originated at the target.
	before := icmpInEchos(t, nsTG)
	fakeICMPRunClient(t, nsCL, self, fakeIP, 0x1234, true)
	if after := icmpInEchos(t, nsTG); after <= before {
		t.Fatal("target saw no echo request; the reply was not relayed from the real target")
	}

	// Phase 2: the target goes silent. The same ping must now fail: any
	// reply would be the gateway kernel's own fake-source echo reply, the
	// exact local-fake-success bug this test exists to gate.
	if out, err := exec.Command("ip", "netns", "exec", nsTG, "sysctl", "-w", "net.ipv4.icmp_echo_ignore_all=1").CombinedOutput(); err != nil {
		t.Fatalf("silence target: %v\n%s", err, out)
	}
	fakeICMPRunClient(t, nsCL, self, fakeIP, 0x5678, false)

	if err := stdin.Close(); err != nil {
		t.Fatalf("close gateway stdin: %v", err)
	}
	select {
	case err := <-waitCh:
		if err != nil {
			t.Fatalf("gateway failed: %v\nstdout:\n%s\nstderr:\n%s", err, gwStdout, gwStderr)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("gateway did not exit after stdin close\nstdout:\n%s\nstderr:\n%s", gwStdout, gwStderr)
	}
}

// fakeICMPRunClient runs the ICMP client child inside the client
// namespace and fails the test if the child's expectation is unmet.
func fakeICMPRunClient(t *testing.T, ns, self, fakeIP string, id int, wantReply bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out := &boundedBuffer{}
	cmd := exec.CommandContext(ctx, "ip", "netns", "exec", ns, self, fakeICMPE2ERunFlag)
	cmd.WaitDelay = 10 * time.Second
	cmd.Env = append(os.Environ(),
		fakeICMPE2ERoleEnv+"="+fakeICMPE2ERoleClient,
		fakeICMPE2EFakeIPEnv+"="+fakeIP,
		fakeICMPE2EIDEnv+"="+strconv.Itoa(id),
		fakeICMPE2EWantEnv+"="+strconv.FormatBool(wantReply),
	)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		t.Fatalf("icmp client (wantReply=%v): %v\n%s", wantReply, err, out)
	}
}

// icmpInEchos reads the namespace's InEchos counter from /proc/net/snmp:
// proof that echo requests really reached the target namespace.
func icmpInEchos(t *testing.T, ns string) int {
	t.Helper()
	out, err := exec.Command("ip", "netns", "exec", ns, "cat", "/proc/net/snmp").Output()
	if err != nil {
		t.Fatalf("read %s /proc/net/snmp: %v", ns, err)
	}
	var hdr, vals []string
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "Icmp:") {
			continue
		}
		if hdr == nil {
			hdr = strings.Fields(line)
			continue
		}
		vals = strings.Fields(line)
		break
	}
	if hdr == nil || vals == nil {
		t.Fatalf("no Icmp counters in %s /proc/net/snmp:\n%s", ns, out)
	}
	for i, name := range hdr {
		if name == "InEchos" && i < len(vals) {
			n, err := strconv.Atoi(vals[i])
			if err != nil {
				t.Fatalf("parse InEchos: %v", err)
			}
			return n
		}
	}
	t.Fatal("InEchos counter not found")
	return 0
}

// fakeICMPNetnsGateway runs inside the gateway namespace. It installs the
// real fake-ip policy route and nftables ruleset, then relays ICMP echo
// for the allocated fake IP to the target namespace. The single "ready"
// line on stdout is the handshake with the parent; the relay lives until
// stdin EOF.
func fakeICMPNetnsGateway(t *testing.T) {
	t.Helper()
	if err := ensureFakeIPRoute(); err != nil {
		t.Fatalf("ensureFakeIPRoute: %v", err)
	}
	iface := os.Getenv(fakeICMPE2EIfEnv)
	if iface == "" {
		t.Fatalf("%s is not set", fakeICMPE2EIfEnv)
	}
	cfg := &Config{CIDR: fakeICMPE2EClusterCIDR, GwIP: "10.23.0.1"}
	if err := ApplyNFT(RenderNFT(cfg, fakeICMPE2ENoUplink, iface, nil)); err != nil {
		t.Fatalf("ApplyNFT: %v", err)
	}
	eng, pool := newFakeUDPTestPolicy(t)
	fake := pool.Allocate("example.com")
	p := newFakeICMP(eng, "10.23.0.1", func(net.IP) string { return "dev1" })
	p.lookupDirect = func(context.Context, string) (net.IP, error) {
		return net.ParseIP(fakeICMPE2ETargetIP), nil
	}
	if err := p.Start("10.23.0.1"); err != nil {
		t.Fatalf("fakeicmp start: %v", err)
	}
	t.Cleanup(p.Close)
	fmt.Printf("ready %s\n", fake.String())
	// The parent closes stdin once both phases finished; returning lets
	// the cleanup close the relay.
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

// fakeICMPNetnsClient runs inside the client namespace and exercises the
// path a VM guest would take: a real ICMP echo client on a connected raw
// socket to the fake IP. It sends three echo requests and, depending on
// PVM_FAKEICMP_E2E_WANT_REPLY, requires at least one echo reply back — or
// total silence, which only holds when the gateway kernel's false local
// echo replies are suppressed.
func fakeICMPNetnsClient(t *testing.T) {
	t.Helper()
	ip := net.ParseIP(os.Getenv(fakeICMPE2EFakeIPEnv))
	id, err := strconv.Atoi(os.Getenv(fakeICMPE2EIDEnv))
	wantReply := os.Getenv(fakeICMPE2EWantEnv) == "true"
	if ip == nil || err != nil || id < 0 || id > 0xffff {
		t.Fatalf("bad fake address %q id %q", os.Getenv(fakeICMPE2EFakeIPEnv), os.Getenv(fakeICMPE2EIDEnv))
	}
	conn, err := net.Dial("ip4:icmp", ip.String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	payload := []byte("pvm-fakeicmp-e2e")
	replied := 0
	for seq := 1; seq <= 3; seq++ {
		if _, err := conn.Write(buildEcho(icmpEchoRequest, id, seq, payload)); err != nil {
			t.Fatalf("write seq %d: %v", seq, err)
		}
		for {
			_ = conn.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
			buf := make([]byte, 2048)
			n, err := conn.Read(buf)
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					break
				}
				t.Fatalf("read: %v", err)
			}
			msg := icmpMessage(buf[:n])
			if len(msg) < 8 || msg[0] != icmpEchoReply || int(binary.BigEndian.Uint16(msg[4:6])) != id {
				continue
			}
			if !bytes.Equal(msg[8:], payload) {
				t.Fatalf("reply payload = %q, want %q", msg[8:], payload)
			}
			replied++
			if wantReply {
				break
			}
		}
		if wantReply && replied > 0 {
			break
		}
	}
	if wantReply && replied == 0 {
		t.Fatal("no echo reply from the fake destination")
	}
	if !wantReply && replied > 0 {
		t.Fatalf("got %d echo replies with the target silent; the gateway faked local success", replied)
	}
}

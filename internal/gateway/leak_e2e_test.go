//go:build !windows

package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const leakRoleEnv = "PVM_LEAK_E2E_ROLE"
const leakRunFlag = "-test.run=^TestKillSwitchUplinkE2ELinux$"

// A working main-table uplink is essential here: a missing uplink would make
// the absence of leaked packets vacuous. The external namespace counts packets
// before its input firewall, and removing pvm must expose every probe family.
func TestKillSwitchUplinkE2ELinux(t *testing.T) {
	switch os.Getenv(leakRoleEnv) {
	case "probe", "marked":
		leakE2EProbe(t, os.Getenv(leakRoleEnv) == "marked")
		return
	case "echo":
		leakE2EEcho(t)
		return
	case "mapped":
		c, err := net.DialTimeout("tcp4", "10.0.2.15:18090", 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		payload := []byte("mapped-reply")
		if _, err = c.Write(payload); err != nil {
			t.Fatal(err)
		}
		reply := make([]byte, len(payload))
		if _, err = io.ReadFull(c, reply); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(payload, reply) {
			t.Fatal("wrong DNAT echo")
		}
		return
	}
	if runtime.GOOS != "linux" {
		t.Skip("network namespaces and nftables require Linux")
	}
	if os.Geteuid() != 0 {
		t.Skip("network namespaces require root")
	}
	for _, tool := range []string{"ip", "nft", "ping", "sysctl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("pvml%d", os.Getpid())
	gw, guest, outer := prefix+"g", prefix+"c", prefix+"o"
	run := func(args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "ip", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("ip %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	for _, ns := range []string{gw, guest, outer} {
		ns := ns
		t.Cleanup(func() { _ = exec.Command("ip", "netns", "del", ns).Run() })
		run("netns", "add", ns)
		run("-n", ns, "link", "set", "lo", "up")
		run("netns", "exec", ns, "sysctl", "-qw", "net.ipv6.conf.all.disable_ipv6=0", "net.ipv6.conf.default.disable_ipv6=0")
	}
	link := func(left, right, leftName, rightName string) {
		t.Helper()
		// Create both ends inside an owned namespace, so a setup failure cannot
		// strand a host link outside cleanup's reach.
		run("-n", left, "link", "add", leftName, "type", "veth", "peer", "name", rightName)
		run("-n", left, "link", "set", rightName, "netns", right)
		run("-n", left, "link", "set", leftName, "up")
		run("-n", right, "link", "set", rightName, "up")
	}
	link(gw, guest, "br0", "guest0")
	link(gw, outer, "upl0", "outside0")
	for _, v := range []struct{ ns, dev, v4, v6 string }{
		{gw, "br0", "10.23.0.1/24", "2001:db8:1::1/64"},
		{guest, "guest0", "10.23.0.2/24", "2001:db8:1::2/64"},
		{gw, "upl0", "10.0.2.15/24", "2001:db8:2::1/64"},
		{outer, "outside0", "10.0.2.2/24", "2001:db8:2::2/64"},
	} {
		run("-n", v.ns, "addr", "add", v.v4, "dev", v.dev)
		run("-n", v.ns, "-6", "addr", "add", v.v6, "dev", v.dev, "nodad")
	}
	run("-n", outer, "addr", "add", "192.0.2.10/32", "dev", "lo")
	run("-n", outer, "-6", "addr", "add", "2001:db8:3::10/128", "dev", "lo", "nodad")
	run("-n", guest, "route", "add", "default", "via", "10.23.0.1")
	run("-n", guest, "-6", "route", "add", "default", "via", "2001:db8:1::1")
	run("-n", gw, "route", "add", "default", "via", "10.0.2.2")
	run("-n", gw, "-6", "route", "add", "default", "via", "2001:db8:2::2")
	run("-n", outer, "route", "add", "10.23.0.0/24", "via", "10.0.2.15")
	run("-n", outer, "-6", "route", "add", "2001:db8:1::/64", "via", "2001:db8:2::1")
	run("netns", "exec", gw, "sysctl", "-qw", "net.ipv4.ip_forward=1", "net.ipv6.conf.all.forwarding=1", "net.ipv4.conf.all.rp_filter=0", "net.ipv4.conf.default.rp_filter=0")
	run("-n", gw, "link", "add", "pvmwg0", "type", "dummy")
	run("-n", gw, "link", "set", "pvmwg0", "up")
	run("-n", gw, "route", "add", "default", "dev", "pvmwg0", "table", "100")
	run("-n", gw, "rule", "add", "from", "10.23.0.0/24", "pref", "100", "lookup", "100")
	nft := func(ns, rules string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "ip", "netns", "exec", ns, "nft", "-f", "-")
		cmd.Stdin = strings.NewReader(rules)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("nft in %s: %v\n%s\n%s", ns, err, out, rules)
		}
	}
	cfg := &Config{CIDR: "10.23.0.0/24", GwIP: "10.23.0.1", DefaultExit: "my-exit", Exits: map[string]ExitDef{"my-exit": {Transport: "udp", MTU: 1420}}}
	rules := RenderNFT(cfg, "upl0", "br0", nil)
	nft(gw, rules)
	probes := []string{"tcp4", "udp4", "icmp4", "dns_udp", "dns_tcp", "ipv6", "slirp"}
	capture := func() {
		t.Helper()
		var s strings.Builder
		s.WriteString("destroy table inet capture\ntable inet capture {\n")
		for _, name := range probes {
			fmt.Fprintf(&s, "counter %s {}\n", name)
		}
		s.WriteString("chain ingress { type filter hook prerouting priority -300; policy accept;\n")
		for _, v := range []struct{ name, expr string }{
			{"tcp4", "ip daddr 192.0.2.10 tcp dport 18080"},
			{"udp4", "ip daddr 192.0.2.10 udp dport 18082"},
			{"icmp4", "ip daddr 192.0.2.10 icmp type echo-request"},
			{"dns_udp", "ip daddr 192.0.2.10 udp dport 53"},
			{"dns_tcp", "ip daddr 192.0.2.10 tcp dport 53"},
			{"ipv6", "ip6 daddr 2001:db8:3::10"},
			{"slirp", "ip daddr 10.0.2.2 udp dport 18082"},
		} {
			fmt.Fprintf(&s, "iifname \"outside0\" %s counter name %s\n", v.expr, v.name)
		}
		s.WriteString("}\n}\n")
		nft(outer, s.String())
	}
	probe := func(ns, role string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "ip", "netns", "exec", ns, self, leakRunFlag)
		cmd.Env = append(os.Environ(), leakRoleEnv+"="+role)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s helper: %v\n%s", role, err, out)
		}
	}
	assertCounts := func(leaking bool) {
		t.Helper()
		var listing struct {
			NFTables []struct {
				Counter *struct {
					Name    string
					Packets uint64
				}
			} `json:"nftables"`
		}
		out := run("netns", "exec", outer, "nft", "-j", "list", "table", "inet", "capture")
		if err := json.Unmarshal([]byte(out), &listing); err != nil {
			t.Fatal(err)
		}
		counts := map[string]uint64{}
		for _, item := range listing.NFTables {
			if item.Counter != nil {
				counts[item.Counter.Name] = item.Counter.Packets
			}
		}
		for _, name := range probes {
			n, ok := counts[name]
			if !ok {
				t.Fatalf("missing capture counter %s", name)
			}
			if leaking && n == 0 {
				t.Errorf("negative control failed to detect %s", name)
			}
			if !leaking && n != 0 {
				t.Errorf("%s leaked %d packets to the physical uplink", name, n)
			}
		}
	}
	for _, phase := range []string{"tunnel_present", "route_missing", "tunnel_removed", "rules_rebuilt"} {
		t.Logf("phase: %s", phase)
		switch phase {
		case "route_missing":
			run("-n", gw, "route", "del", "default", "table", "100")
		case "tunnel_removed":
			run("-n", gw, "link", "del", "pvmwg0")
		case "rules_rebuilt":
			nft(gw, rules)
		}
		capture()
		probe(guest, "probe")
		assertCounts(false)
		capture()
		probe(gw, "marked")
		assertCounts(false)
	}
	// Remove all enforcement, including DNS interception, only inside this
	// disposable gateway. Every counter must then see real packets.
	t.Log("phase: negative_control")
	nft(gw, "delete table inet pvm\n")
	capture()
	probe(guest, "probe")
	assertCounts(true)
	capture()
	probe(gw, "marked")
	assertCounts(true)
	// Check the actual conntrack reply semantics, not just rendered text.
	// The client is the simulated slirp host and the server is an exit VM.
	cfg.Maps = []MapEntry{{HostPort: 18090, GuestPort: 18090, VMIP: "10.23.0.2", Proto: "tcp"}}
	mappedRules := RenderNFT(cfg, "upl0", "br0", nil)
	nft(gw, mappedRules)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	echo := exec.CommandContext(ctx, "ip", "netns", "exec", guest, self, leakRunFlag)
	echo.Env = append(os.Environ(), leakRoleEnv+"=echo")
	stderr := &realipE2ETailBuf{}
	echo.Stderr = stderr
	stdout, err := echo.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := echo.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = echo.Wait() }()
	ready := make(chan bool, 1)
	go func() { scanner := bufio.NewScanner(stdout); ready <- scanner.Scan() && scanner.Text() == "ready echo" }()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatalf("echo helper failed: %s", stderr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("echo helper readiness timeout")
	}
	mapped := func(wantSuccess bool) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "ip", "netns", "exec", outer, self, leakRunFlag)
		cmd.Env = append(os.Environ(), leakRoleEnv+"=mapped")
		out, err := cmd.CombinedOutput()
		if (err == nil) != wantSuccess {
			t.Fatalf("DNAT reply success=%v, want %v: %s", err == nil, wantSuccess, out)
		}
	}
	mapped(true)
	exception := "    iifname \"br0\" oifname \"upl0\" ip daddr 10.0.2.0/24 ct direction reply ct status dnat accept\n"
	if !strings.Contains(mappedRules, exception) {
		t.Fatal("missing DNAT reply exception")
	}
	nft(gw, strings.Replace(mappedRules, exception, "", 1))
	mapped(false)
	nft(gw, mappedRules)
	mapped(true)
}

func leakE2EProbe(t *testing.T, marked bool) {
	t.Helper()
	d := net.Dialer{Timeout: 200 * time.Millisecond}
	if marked {
		d.Control = func(_, _ string, c syscall.RawConn) error {
			var setErr error
			err := c.Control(func(fd uintptr) {
				setErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, 36 /* Linux SO_MARK */, int(fwMarkBase))
			})
			if err != nil {
				return err
			}
			return setErr
		}
	}
	for _, target := range []struct{ network, addr string }{
		{"tcp4", "192.0.2.10:18080"}, {"udp4", "192.0.2.10:18082"},
		{"udp4", "192.0.2.10:53"}, {"tcp4", "192.0.2.10:53"},
		{"udp6", "[2001:db8:3::10]:18082"}, {"udp4", "10.0.2.2:18082"},
	} {
		c, err := d.Dial(target.network, target.addr)
		if err != nil {
			continue
		}
		_ = c.SetWriteDeadline(time.Now().Add(time.Second))
		payload := []byte("pvm-leak-probe")
		if strings.HasSuffix(target.addr, ":53") {
			// An actual A query for example.com, including TCP DNS framing.
			payload = []byte{0x12, 0x34, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 3, 'c', 'o', 'm', 0, 0, 1, 0, 1}
			if target.network == "tcp4" {
				var size [2]byte
				binary.BigEndian.PutUint16(size[:], uint16(len(payload)))
				payload = append(size[:], payload...)
			}
		}
		_, _ = c.Write(payload)
		_ = c.Close()
	}
	args := []string{"-n", "-c", "1", "-W", "1"}
	if marked {
		args = append(args, "-m", strconv.Itoa(int(fwMarkBase)))
	}
	args = append(args, "192.0.2.10")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "ping", args...).Run()
}

func leakE2EEcho(t *testing.T) {
	ln, err := net.Listen("tcp4", "10.23.0.2:18090")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	fmt.Println("ready echo")
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
	}
}

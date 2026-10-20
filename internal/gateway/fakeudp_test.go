package gateway

import (
	"context"
	"net"
	"runtime"
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

package exit

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"runtime"
	"testing"
	"time"
)

func startUserspaceServer(t *testing.T, transports string) *Server {
	t.Helper()
	return startTestServer(t, ServerConfig{
		Listen:     "127.0.0.1:0",
		Token:      "test-token",
		StateDir:   t.TempDir(),
		Transports: transports,
		NAT:        NATUserspace,
	})
}

// hostLANIPv4 returns a non-loopback IPv4 of the host; the userspace NAT
// dials it as the flow destination (gVisor drops loopback-range flows).
func hostLANIPv4(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.To4() != nil && !ipnet.IP.IsLoopback() {
			return ipnet.IP.String()
		}
	}
	t.Skip("no non-loopback IPv4 on host")
	return ""
}

// udpEchoOS runs a UDP echo server on the host (all interfaces).
func udpEchoOS(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, src, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = c.WriteToUDP(append([]byte("echo:"), buf[:n]...), src)
		}
	}()
	return c.LocalAddr().(*net.UDPAddr).Port
}

// Full path: WG handshake against the userspace backend, then UDP and TCP
// flows forwarded by net.Dial to host services.
func TestUserspaceNATEndToEnd(t *testing.T) {
	echoPort := udpEchoOS(t)
	httpSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "pvm-userspace-ok")
	}))
	httpSrv.Listener, _ = net.Listen("tcp", ":0")
	httpSrv.Start()
	defer httpSrv.Close()
	_, httpPortStr, _ := net.SplitHostPort(httpSrv.Listener.Addr().String())
	var httpPort int
	fmt.Sscanf(httpPortStr, "%d", &httpPort)
	hostIP := hostLANIPv4(t)

	for _, transport := range []string{"udp", "tcp"} {
		t.Run(transport, func(t *testing.T) {
			srv := startUserspaceServer(t, transport)
			entry := enrollViaTOFU(t, srv, transport)
			serverIP := "10.99.0.1"
			if transport == "tcp" {
				serverIP = "10.100.0.1"
			}

			tun, err := DialEntry(entry)
			if err != nil {
				t.Fatal(err)
			}
			defer tun.Close()

			ctx, cancel := context.WithTimeout(context.Background(), testLongTimeout)
			defer cancel()
			if err := tun.WaitHandshake(ctx, serverIP); err != nil {
				t.Fatal(err)
			}

			// UDP through the tunnel -> host echo server (retry: UDP is lossy)
			c, err := tun.Net.DialUDPAddrPort(netip.AddrPort{}, netip.AddrPortFrom(netip.MustParseAddr(hostIP), uint16(echoPort)))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			buf := make([]byte, 64)
			var echoed bool
			for attempt := 0; attempt < 5 && !echoed; attempt++ {
				if _, err := c.Write([]byte("ping")); err != nil {
					t.Fatal(err)
				}
				_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
				n, err := c.Read(buf)
				if err != nil {
					continue
				}
				if string(buf[:n]) != "echo:ping" {
					t.Fatalf("udp echo = %q", buf[:n])
				}
				echoed = true
			}
			if !echoed {
				t.Fatal("udp via userspace NAT: no echo after 5 attempts")
			}

			// TCP through the tunnel -> host http server
			body, err := tun.httpGet(ctx, netip.MustParseAddr(hostIP), uint16(httpPort), hostIP, "/")
			if err != nil {
				t.Fatalf("tcp via userspace NAT: %v", err)
			}
			if body != "pvm-userspace-ok" {
				t.Fatalf("http body = %q", body)
			}
		})
	}
}

// ICMP echo through the userspace NAT relay. The relay's host side needs
// a Linux ping socket (net.ipv4.ping_group_range); without it the relay
// fails closed by design, so the test skips there - it cannot pass.
func TestUserspaceNATICMP(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("icmp relay e2e requires linux ping sockets")
	}
	hostIP := hostLANIPv4(t)
	probe, err := net.DialTimeout("ip4:icmp", hostIP, 3*time.Second)
	if err != nil {
		t.Skipf("ping socket unavailable: %v", err)
	}
	probe.Close()

	srv := startUserspaceServer(t, "udp")
	entry := enrollViaTOFU(t, srv, "udp")
	tun, err := DialEntry(entry)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()
	ctx, cancel := context.WithTimeout(context.Background(), testLongTimeout)
	defer cancel()
	if err := tun.WaitHandshake(ctx, "10.99.0.1"); err != nil {
		t.Fatal(err)
	}

	pc, err := tun.Net.DialPingAddr(netip.Addr{}, netip.MustParseAddr(hostIP))
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	// The client stack's icmp endpoint overwrites ident and recomputes the
	// checksum; sequence and payload pass through, so match on those.
	msg := make([]byte, 8+4)
	msg[0] = 8 // echo request
	binary.BigEndian.PutUint16(msg[6:8], 1)
	copy(msg[8:], "pvm!")
	if _, err := pc.Write(msg); err != nil {
		t.Fatal(err)
	}
	_ = pc.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 2048)
	n, err := pc.Read(buf)
	if err != nil {
		t.Fatalf("icmp echo via userspace NAT: %v", err)
	}
	reply := buf[:n]
	if len(reply) < 8+4 || reply[0] != 0 || reply[1] != 0 {
		t.Fatalf("not an echo reply: % x", reply)
	}
	if got := binary.BigEndian.Uint16(reply[6:8]); got != 1 {
		t.Fatalf("reply seq = %d, want 1", got)
	}
	if string(reply[8:12]) != "pvm!" {
		t.Fatalf("reply payload = %q", reply[8:n])
	}
}

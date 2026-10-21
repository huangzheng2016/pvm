package exit

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/huangzheng2016/pvm/internal/tunnel"
	"golang.zx2c4.com/wireguard/tun"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

// inProcessServer starts an exit-agent with netstack TUNs (no root, no OS
// network changes) and returns the server plus its netstack nets per tun name.
func inProcessServer(t *testing.T, transports string) (*Server, map[string]*netstack.Net) {
	t.Helper()
	nets := map[string]*netstack.Net{}
	factory := func(name string, mtu int) (tun.Device, error) {
		var ip netip.Addr
		switch name {
		case "pvmexit0":
			ip = netip.MustParseAddr("10.99.0.1")
		case "pvmexit1":
			ip = netip.MustParseAddr("10.100.0.1")
		default:
			return nil, fmt.Errorf("unexpected tun %q", name)
		}
		td, tn, err := netstack.CreateNetTUN([]netip.Addr{ip}, nil, mtu)
		if err != nil {
			return nil, err
		}
		nets[name] = tn
		return td, nil
	}
	srv := startTestServer(t, ServerConfig{
		Listen:     "127.0.0.1:0",
		Token:      "test-token",
		StateDir:   t.TempDir(),
		Transports: transports,
		TunFactory: factory,
	})
	return srv, nets
}

func freePort(t *testing.T, network string) int {
	t.Helper()
	if network == "udp" {
		c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		return c.LocalAddr().(*net.UDPAddr).Port
	}
	l, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func startTestServer(t *testing.T, cfg ServerConfig) *Server {
	t.Helper()
	for attempt := 0; attempt < 5; attempt++ {
		cfg.UDPPort = freePort(t, "udp")
		cfg.TCPPort = freePort(t, "tcp")
		srv, err := NewServer(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := srv.Start(); err == nil {
			t.Cleanup(srv.Close)
			return srv
		} else {
			srv.Close()
			if !strings.Contains(err.Error(), "address already in use") {
				t.Fatal(err)
			}
		}
	}
	t.Fatal("could not bind test WireGuard ports after 5 attempts")
	return nil
}

// enrollViaTOFU mimics the CLI: probe fingerprint, pin it, enroll, build an entry.
func enrollViaTOFU(t *testing.T, srv *Server, transport string) *Entry {
	t.Helper()
	serverAddr := srv.ControlAddr()
	fp, err := ProbeFingerprint(serverAddr)
	if err != nil {
		t.Fatal(err)
	}
	priv, err := tunnel.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	pub, err := tunnel.PublicKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), testOpTimeout)
	defer cancel()
	resp, err := NewClient(serverAddr, "test-token", fp).Enroll(ctx, pub, "test-host", transport)
	if err != nil {
		t.Fatal(err)
	}
	return &Entry{
		Name:         "it",
		Server:       serverAddr,
		Token:        "test-token",
		Transport:    transport,
		Fingerprint:  fp,
		WGPrivateKey: priv,
		WGPublicKey:  pub,
		ServerPubKey: resp.ServerPubKey,
		TunnelIP:     resp.TunnelIP,
		MTU:          resp.MTU,
		Endpoint:     resp.Endpoint,
	}
}

// udpEcho runs a UDP echo server on the server's netstack at ip:port.
func udpEcho(t *testing.T, tn *netstack.Net, ip string, port uint16) {
	t.Helper()
	lc, err := tn.ListenUDPAddrPort(netip.AddrPortFrom(netip.MustParseAddr(ip), port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lc.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, src, err := lc.ReadFrom(buf)
			if err != nil {
				return
			}
			msg := append([]byte("echo:"), buf[:n]...)
			if _, err := lc.WriteTo(msg, src); err != nil {
				return
			}
		}
	}()
}

func TestEndToEndInProcess(t *testing.T) {
	for _, transport := range []string{"udp", "tcp"} {
		t.Run(transport, func(t *testing.T) {
			srv, nets := inProcessServer(t, transport)

			entry := enrollViaTOFU(t, srv, transport)
			wantSubnet := "10.99.0."
			serverTun := "pvmexit0"
			serverIP := "10.99.0.1"
			if transport == "tcp" {
				wantSubnet = "10.100.0."
				serverTun = "pvmexit1"
				serverIP = "10.100.0.1"
			}
			if !strings.HasPrefix(entry.TunnelIP, wantSubnet) {
				t.Fatalf("tunnel ip %s not in %s*", entry.TunnelIP, wantSubnet)
			}

			udpEcho(t, nets[serverTun], serverIP, 9999)

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

			// application traffic through the tunnel
			c, err := tun.Net.DialUDPAddrPort(netip.AddrPort{}, netip.AddrPortFrom(netip.MustParseAddr(serverIP), 9999))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if _, err := c.Write([]byte("ping")); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 64)
			deadline := time.Now().Add(10 * time.Second)
			for {
				_ = c.SetReadDeadline(deadline)
				n, err := c.Read(buf)
				if err != nil {
					t.Fatal(err)
				}
				if string(buf[:n]) == "echo:ping" {
					break
				}
			}

			// revoke: peer disappears from the store
			if err := NewClient(entry.Server, "test-token", entry.Fingerprint).DeletePeer(ctx, entry.TunnelIP); err != nil {
				t.Fatal(err)
			}
			for _, p := range srv.store.List() {
				if p.IP == entry.TunnelIP {
					t.Fatalf("peer %s still present after delete", entry.TunnelIP)
				}
			}
		})
	}
}

func TestFingerprintPinningRejectsWrongCert(t *testing.T) {
	srv, _ := inProcessServer(t, "udp")
	ctx, cancel := context.WithTimeout(context.Background(), testOpTimeout)
	defer cancel()
	err := NewClient(srv.ControlAddr(), "test-token", "0000000000000000000000000000000000000000000000000000000000000000").Health(ctx)
	if err == nil {
		t.Fatal("want fingerprint mismatch error")
	}
	// correct token but no auth needed for health; also verify auth gate on enroll
	if _, err := NewClient(srv.ControlAddr(), "wrong-token", "").Enroll(ctx, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "h", "udp"); err == nil {
		t.Fatal("want unauthorized error")
	}
}

func TestServerRestartRestoresPeers(t *testing.T) {
	stateDir := t.TempDir()
	factory := func(name string, mtu int) (tun.Device, error) {
		td, _, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("10.99.0.1")}, nil, mtu)
		if err != nil {
			return nil, err
		}
		return td, nil
	}
	start := func(t *testing.T) (*Server, int) {
		wgPort := freePort(t, "udp")
		srv, err := NewServer(ServerConfig{
			Listen:     "127.0.0.1:0",
			Token:      "test-token",
			StateDir:   stateDir,
			UDPPort:    wgPort,
			Transports: "udp",
			TunFactory: factory,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := srv.Start(); err != nil {
			t.Fatal(err)
		}
		return srv, wgPort
	}

	srv1, _ := start(t)
	entry := enrollViaTOFU(t, srv1, "udp")
	srv1.Close()

	// after a restart the device is fresh; peers must be replayed from the
	// store or the handshake is rejected
	srv2, wgPort := start(t)
	defer srv2.Close()
	entry.Server = srv2.ControlAddr()
	entry.Endpoint = net.JoinHostPort("127.0.0.1", itoa(wgPort))

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
}

package exit

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/tunnel"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

// Tunnel is a live userspace WireGuard client connection.
type Tunnel struct {
	Net *netstack.Net
	Dev *device.Device
}

// DialEntry brings up a netstack WireGuard client from a stored entry —
// no OS TUN, no root. Call Close when done.
func DialEntry(e *Entry) (*Tunnel, error) {
	ip, err := netip.ParseAddr(e.TunnelIP)
	if err != nil {
		return nil, fmt.Errorf("bad tunnel ip %q: %w", e.TunnelIP, err)
	}
	mtu := e.MTU
	if mtu == 0 {
		mtu = MTUFor(e.Transport)
	}
	dns := netip.MustParseAddr("1.1.1.1")
	tunDev, tn, err := netstack.CreateNetTUN([]netip.Addr{ip}, []netip.Addr{dns}, mtu)
	if err != nil {
		return nil, err
	}
	dev, err := tunnel.NewDevice(tunDev, tunnel.BindFor(e.Transport), false)
	if err != nil {
		tunDev.Close()
		return nil, err
	}
	privHex, err := tunnel.Hex(e.WGPrivateKey)
	if err != nil {
		dev.Close()
		return nil, err
	}
	pubHex, err := tunnel.Hex(e.ServerPubKey)
	if err != nil {
		dev.Close()
		return nil, err
	}
	conf := fmt.Sprintf(`private_key=%s
public_key=%s
endpoint=%s
allowed_ip=0.0.0.0/0
persistent_keepalive_interval=15
`, privHex, pubHex, e.Endpoint)
	if err := dev.IpcSet(conf); err != nil {
		dev.Close()
		return nil, fmt.Errorf("configure device: %w", err)
	}
	return &Tunnel{Net: tn, Dev: dev}, nil
}

// Close shuts the tunnel down.
func (t *Tunnel) Close() {
	t.Dev.Close()
}

// handshakeTimeout bounds the internal wait in WaitHandshake (relaxed in
// -race test runs).
var handshakeTimeout = 30 * time.Second

// httpTimeout bounds single HTTP requests through the tunnel.
var httpTimeout = 20 * time.Second

// WaitHandshake triggers traffic towards the server's tunnel IP and waits
// until the WireGuard handshake completes (or ctx expires).
func (t *Tunnel) WaitHandshake(ctx context.Context, serverTunnelIP string) error {
	// Any outbound packet forces a handshake; no reply is needed. Keep the
	// conn open so queued packets are not dropped by an early Close.
	c, err := t.Net.DialUDPAddrPort(netip.AddrPort{}, netip.AddrPortFrom(netip.MustParseAddr(serverTunnelIP), 9))
	if err != nil {
		return fmt.Errorf("dial trigger: %w", err)
	}
	defer c.Close()
	deadline := time.Now().Add(handshakeTimeout)
	for {
		_, _ = c.Write([]byte("knock"))
		time.Sleep(300 * time.Millisecond)
		if t.handshakeDone() {
			return nil
		}
		if time.Now().After(deadline) {
			out, _ := t.Dev.IpcGet()
			return fmt.Errorf("wireguard handshake timeout; client ipc:\n%s", out)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
	}
}

func (t *Tunnel) handshakeDone() bool {
	out, err := t.Dev.IpcGet()
	if err != nil {
		return false
	}
	for line := range strings.Lines(out) {
		if k, v, ok := strings.Cut(line, "="); ok && k == "last_handshake_time_sec" {
			n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			return n > 0
		}
	}
	return false
}

// FetchExitIP requests http://ifconfig.me/ip through the tunnel and returns
// the observed public IP — proof the VPS NATs our traffic.
func (t *Tunnel) FetchExitIP(ctx context.Context) (string, error) {
	addrs, err := t.Net.LookupHost("ifconfig.me")
	if err != nil || len(addrs) == 0 {
		return "", fmt.Errorf("dns via tunnel: %v", err)
	}
	var lastErr error
	for _, a := range addrs {
		ip, err := netip.ParseAddr(a)
		if err != nil || !ip.Is4() {
			continue
		}
		body, err := t.httpGet(ctx, ip, 80, "ifconfig.me", "/ip")
		if err != nil {
			lastErr = err
			continue
		}
		return strings.TrimSpace(body), nil
	}
	return "", fmt.Errorf("fetch exit ip: %v", lastErr)
}

func (t *Tunnel) httpGet(ctx context.Context, ip netip.Addr, port uint16, host, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	conn, err := t.Net.DialContextTCPAddrPort(ctx, netip.AddrPortFrom(ip, port))
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: pvm-exit-test\r\nConnection: close\r\n\r\n", path, host); err != nil {
		return "", err
	}
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		return "", err
	}
	if !strings.Contains(status, " 200") {
		return "", fmt.Errorf("http status: %s", strings.TrimSpace(status))
	}
	// skip headers
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return "", err
		}
		if line == "\r\n" {
			break
		}
	}
	b, err := io.ReadAll(io.LimitReader(br, 4096))
	return string(b), err
}

// download streams an HTTP GET through the tunnel, discarding the body and
// reporting received bytes and wall time (speed test).
func (t *Tunnel) download(ctx context.Context, ip netip.Addr, port uint16, path string) (int64, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	start := time.Now()
	conn, err := t.Net.DialContextTCPAddrPort(ctx, netip.AddrPortFrom(ip, port))
	if err != nil {
		return 0, 0, err
	}
	defer conn.Close()
	host := ip.String()
	if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: pvm-speedtest\r\nConnection: close\r\n\r\n", path, host); err != nil {
		return 0, 0, err
	}
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		return 0, 0, err
	}
	if !strings.Contains(status, " 200") {
		return 0, 0, fmt.Errorf("http status: %s", strings.TrimSpace(status))
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return 0, 0, err
		}
		if line == "\r\n" {
			break
		}
	}
	n, err := io.Copy(io.Discard, br)
	return n, time.Since(start), err
}

// Ping fetches /ping from the tunnel speed endpoint (caller times the RTT).
func (t *Tunnel) Ping(ctx context.Context, serverIP netip.Addr) (string, error) {
	return t.httpGet(ctx, serverIP, SpeedPort, serverIP.String(), "/ping")
}

// Download fetches path from the tunnel speed endpoint, reporting bytes
// received and wall time.
func (t *Tunnel) Download(ctx context.Context, serverIP netip.Addr, path string) (int64, time.Duration, error) {
	return t.download(ctx, serverIP, SpeedPort, path)
}

package cli

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/huangzheng2016/pvm/internal/exit"
	"golang.zx2c4.com/wireguard/tun"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

func TestExportedConfigWireGuardDataPlane(t *testing.T) {
	ln, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	wgPort := ln.LocalAddr().(*net.UDPAddr).Port
	ln.Close()

	serverIP := netip.MustParseAddr("10.99.0.1")
	var serverNet *netstack.Net
	srv, err := exit.NewServer(exit.ServerConfig{
		Listen:     "127.0.0.1:0",
		Token:      "test-token",
		StateDir:   t.TempDir(),
		UDPPort:    wgPort,
		Transports: "udp",
		TunFactory: func(name string, mtu int) (tun.Device, error) {
			td, tn, err := netstack.CreateNetTUN([]netip.Addr{serverIP}, nil, mtu)
			if err == nil {
				serverNet = tn
			}
			return td, err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)

	fp, err := exit.ProbeFingerprint(srv.ControlAddr())
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("PVM_HOME", home)
	if err := exit.SaveEntry(filepath.Join(home, "exits"), &exit.Entry{
		Name:        "my-exit",
		Server:      srv.ControlAddr(),
		Token:       "test-token",
		Transport:   "udp",
		Fingerprint: fp,
	}); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "standard.conf")
	if code := exitClientConfigTo([]string{"my-exit", "-o", output}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("client-config exit code = %d", code)
	}
	if wgQuick, err := exec.LookPath("wg-quick"); err == nil {
		if out, err := exec.Command(wgQuick, "strip", output).CombinedOutput(); err != nil {
			t.Fatalf("official wg-quick rejected exported config: %v: %s", err, out)
		}
	}

	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for line := range strings.Lines(string(data)) {
		k, v, ok := strings.Cut(strings.TrimSpace(line), " = ")
		if ok {
			values[k] = v
		}
	}
	mtu, err := strconv.Atoi(values["MTU"])
	if err != nil {
		t.Fatal(err)
	}
	client, err := exit.DialEntry(&exit.Entry{
		Transport:    "udp",
		WGPrivateKey: values["PrivateKey"],
		ServerPubKey: values["PublicKey"],
		TunnelIP:     strings.TrimSuffix(values["Address"], "/32"),
		Endpoint:     values["Endpoint"],
		MTU:          mtu,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	echo, err := serverNet.ListenUDPAddrPort(netip.AddrPortFrom(serverIP, 9999))
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		buf := make([]byte, 128)
		for {
			n, src, err := echo.ReadFrom(buf)
			if err != nil {
				return
			}
			if _, err := echo.WriteTo(append([]byte("echo:"), buf[:n]...), src); err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.WaitHandshake(ctx, serverIP.String()); err != nil {
		t.Fatal(err)
	}
	conn, err := client.Net.DialUDPAddrPort(netip.AddrPort{}, netip.AddrPortFrom(serverIP, 9999))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("standard-client")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 128)
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buf[:n]); got != "echo:standard-client" {
		t.Fatalf("echo = %q", got)
	}
}

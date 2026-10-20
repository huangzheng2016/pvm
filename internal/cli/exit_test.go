package cli

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/huangzheng2016/pvm/internal/exit"
	"github.com/huangzheng2016/pvm/internal/tunnel"
)

func TestObservedIPMatchesHost(t *testing.T) {
	if ok, err := observedIPMatchesHost("192.0.2.1", "192.0.2.1", nil); err != nil || !ok {
		t.Fatalf("literal ip should match: %v, %v", ok, err)
	}
	resolve := func(host string) ([]net.IP, error) {
		if host != "exit.example" {
			t.Fatalf("unexpected host %q", host)
		}
		return []net.IP{net.ParseIP("192.0.2.1"), net.ParseIP("2001:db8::1")}, nil
	}
	if ok, err := observedIPMatchesHost("exit.example", "2001:db8::1", resolve); err != nil || !ok {
		t.Fatalf("one of A/AAAA should match: %v, %v", ok, err)
	}
	if ok, err := observedIPMatchesHost("exit.example", "192.0.2.99", resolve); err != nil || ok {
		t.Fatalf("different ip must not match: %v, %v", ok, err)
	}
	if _, err := observedIPMatchesHost("exit.example", "192.0.2.1", func(string) ([]net.IP, error) {
		return nil, errors.New("dns down")
	}); err == nil {
		t.Fatal("resolver error must be returned")
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns the
// exit code plus everything printed.
func captureStdout(t *testing.T, fn func() int) (int, string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()
	code := fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	return code, string(out)
}

// exitTest against a real in-process exit-agent (userspace NAT): handshake,
// TCP /ping, UDP probe, and ICMP probe must all report ok without touching
// any third-party service.
func TestExitTestOwnAgentProbes(t *testing.T) {
	ln, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	wgPort := ln.LocalAddr().(*net.UDPAddr).Port
	ln.Close()

	srv, err := exit.NewServer(exit.ServerConfig{
		Listen:     "127.0.0.1:0",
		Token:      "test-token",
		StateDir:   t.TempDir(),
		UDPPort:    wgPort,
		Transports: "udp",
		NAT:        exit.NATUserspace,
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
	priv, err := tunnel.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	pub, err := tunnel.PublicKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := exit.NewClient(srv.ControlAddr(), "test-token", fp).Enroll(ctx, pub, "cli-test", "udp")
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("PVM_HOME", home)
	if err := exit.SaveEntry(filepath.Join(home, "exits"), &exit.Entry{
		Name:         "my-exit",
		Server:       srv.ControlAddr(),
		Token:        "test-token",
		Transport:    "udp",
		Fingerprint:  fp,
		WGPrivateKey: priv,
		WGPublicKey:  pub,
		ServerPubKey: resp.ServerPubKey,
		TunnelIP:     resp.TunnelIP,
		ServerIP:     resp.ServerIP,
		MTU:          resp.MTU,
		Endpoint:     resp.Endpoint,
	}); err != nil {
		t.Fatal(err)
	}

	code, out := captureStdout(t, func() int { return exitTest([]string{"my-exit"}) })
	if code != 0 {
		t.Fatalf("exitTest code = %d, output:\n%s", code, out)
	}
	for _, want := range []string{
		"wireguard handshake: ok",
		"tcp /ping: ok",
		"udp probe: ok",
		"icmp probe: ok",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ifconfig.me") {
		t.Errorf("output still references ifconfig.me:\n%s", out)
	}
}

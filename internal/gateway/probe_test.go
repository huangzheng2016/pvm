package gateway

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/huangzheng2016/pvm/internal/exit"
)

func TestExitProbe(t *testing.T) {
	stubIPRule(t)
	stubIPRoute(t)
	target := ""
	orig := exitProbeDial
	exitProbeDial = func(ctx context.Context, network, address string, mark int) (net.Conn, error) {
		switch network {
		case "tcp4":
			return net.DialTimeout("tcp", target, time.Second)
		case "udp4":
			client, server := net.Pipe()
			go func() {
				defer server.Close()
				buf := make([]byte, 20)
				if _, err := server.Read(buf); err == nil {
					_, _ = server.Write(buf)
				}
			}()
			return client, nil
		case "ip4:icmp":
			client, server := net.Pipe()
			go func() {
				defer server.Close()
				buf := make([]byte, 12)
				if _, err := server.Read(buf); err == nil {
					buf[0] = 0
					_, _ = server.Write(buf)
				}
			}()
			return client, nil
		default:
			return nil, fmt.Errorf("unexpected probe network %q", network)
		}
	}
	t.Cleanup(func() { exitProbeDial = orig })

	serve := func(status string) string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		go func() {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			buf := make([]byte, 4096)
			_, _ = conn.Read(buf)
			_, _ = conn.Write([]byte("HTTP/1.1 " + status + "\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
		}()
		return ln.Addr().String()
	}

	target = serve("200 OK")
	m := newExitManager()
	if err := m.probe(&exitTunnel{serverTunIP: "203.0.113.1", idx: 0}); err != nil {
		t.Fatalf("healthy probe failed: %v", err)
	}

	target = serve("503 Service Unavailable")
	err := m.probe(&exitTunnel{serverTunIP: "203.0.113.1", idx: 0})
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("unhealthy probe error = %v", err)
	}

	if err := m.probe(&exitTunnel{def: ExitDef{Provider: exit.ProviderWARP}}); err != nil {
		t.Fatalf("WARP probe must use handshake-only health: %v", err)
	}
	if err := m.probe(&exitTunnel{}); err == nil {
		t.Fatal("missing server tunnel address must fail")
	}
}

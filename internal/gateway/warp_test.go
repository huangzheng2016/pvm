package gateway

import (
	"testing"

	"golang.zx2c4.com/wireguard/device"
)

func TestTunnelDNSAddr(t *testing.T) {
	m := newExitManager()
	m.tuns["warp-test"] = &exitTunnel{def: ExitDef{DNS: "1.1.1.1"}, dev: &device.Device{}}
	addr, ok := m.TunnelDNSAddr("warp-test")
	if !ok || addr != "1.1.1.1:53" {
		t.Fatalf("TunnelDNSAddr = %q, %v", addr, ok)
	}
	m.tuns["down"] = &exitTunnel{def: ExitDef{DNS: "1.1.1.1"}}
	if _, ok := m.TunnelDNSAddr("down"); ok {
		t.Fatal("down tunnel must not have a resolver")
	}
	if _, ok := m.TunnelDNSAddr("missing"); ok {
		t.Fatal("unknown exit must not have a resolver")
	}
}

func TestResolveEndpoint(t *testing.T) {
	endpoint, host, err := resolveEndpoint("203.0.113.1:2408")
	if err != nil || endpoint != "203.0.113.1:2408" || host != "203.0.113.1" {
		t.Fatalf("literal endpoint = %q, %q, %v", endpoint, host, err)
	}
	endpoint, host, err = resolveEndpoint("localhost:2408")
	if err != nil || endpoint != "127.0.0.1:2408" || host != "127.0.0.1" {
		t.Fatalf("resolved endpoint = %q, %q, %v", endpoint, host, err)
	}
	if _, _, err := resolveEndpoint("[2001:db8::1]:2408"); err == nil {
		t.Fatal("IPv6-only endpoint must be rejected by the IPv4 gateway")
	}
}

package cli

import (
	"errors"
	"net"
	"testing"
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

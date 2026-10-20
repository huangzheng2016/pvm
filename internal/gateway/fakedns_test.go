package gateway

import (
	"context"
	"errors"
	"net"
	"testing"

	gwdns "github.com/huangzheng2016/pvm/internal/gw/dns"
	gwpolicy "github.com/huangzheng2016/pvm/internal/gw/policy"
	"github.com/huangzheng2016/pvm/internal/gw/rules"
)

func newFakeDNSTest(t *testing.T) *fakeDNS {
	t.Helper()
	f, err := newFakeDNS("192.0.2.1", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	return f
}

func TestExitForSource(t *testing.T) {
	eng, err := rules.NewEngine("", rules.DefaultInternal())
	if err != nil {
		t.Fatal(err)
	}
	pool, err := gwdns.NewPool("198.18.0.0/16", 16)
	if err != nil {
		t.Fatal(err)
	}
	pol := gwpolicy.New(eng, pool)
	pol.SetDefaultExit("my-exit")
	pol.SetVMExit("dev1", "jp")
	pol.SetVMExit("dev2", "direct")
	vmByIP := func(ip net.IP) string {
		switch ip.String() {
		case "100.99.1.10":
			return "dev1"
		case "100.99.1.11":
			return "dev2"
		}
		return ""
	}
	exitFor := exitForSource(pol, vmByIP)

	if got := exitFor(net.ParseIP("100.99.1.10")); got != "jp" {
		t.Fatalf("dev1: %q", got)
	}
	if got := exitFor(net.ParseIP("100.99.1.11")); got != "" {
		t.Fatalf("explicit direct vm must override the default exit: %q", got)
	}
	if got := exitFor(net.ParseIP("100.99.1.12")); got != "my-exit" {
		t.Fatalf("unknown vm must fall to the cluster default: %q", got)
	}
	if got := exitFor(nil); got != "my-exit" {
		t.Fatalf("nil source must fall to the cluster default: %q", got)
	}
	if got := exitForSource(nil, vmByIP)(net.ParseIP("100.99.1.10")); got != "" {
		t.Fatalf("nil policy must be direct: %q", got)
	}
}

func TestFakeDNSExitResolverRealViaAgentExit(t *testing.T) {
	f := newFakeDNSTest(t)
	var gotDomain, gotExit, gotDC, gotDNS string
	var gotMark int
	f.resolveExit = func(_ context.Context, domain, exitName string, mark int, dcAddr, dnsAddr string) (net.IP, error) {
		gotDomain, gotExit, gotMark, gotDC, gotDNS = domain, exitName, mark, dcAddr, dnsAddr
		return net.ParseIP("203.0.113.9"), nil
	}
	f.SetExitHooks(nil, nil,
		func(string) int { return 5 },
		func(string) (string, bool) { return "10.7.0.1:18080", true },
		func(string) (string, bool) { return "", false })

	ip, mode, err := f.exitResolver()(context.Background(), "api.example.com", "my-exit")
	if err != nil || mode != gwdns.ExitResolveReal || ip.String() != "203.0.113.9" {
		t.Fatalf("resolver = %v, %v, %v", ip, mode, err)
	}
	if gotDomain != "api.example.com" || gotExit != "my-exit" || gotMark != 5 || gotDC != "10.7.0.1:18080" || gotDNS != "" {
		t.Fatalf("lookup args = %q %q %d %q %q", gotDomain, gotExit, gotMark, gotDC, gotDNS)
	}
}

func TestFakeDNSExitResolverRealViaWarpDNS(t *testing.T) {
	f := newFakeDNSTest(t)
	var gotDNS string
	f.resolveExit = func(_ context.Context, domain, exitName string, mark int, dcAddr, dnsAddr string) (net.IP, error) {
		gotDNS = dnsAddr
		return net.ParseIP("203.0.113.10"), nil
	}
	f.SetExitHooks(nil, nil,
		func(string) int { return 6 },
		func(string) (string, bool) { return "", false },
		func(string) (string, bool) { return "203.0.113.53:53", true })

	ip, mode, err := f.exitResolver()(context.Background(), "api.example.com", "my-warp")
	if err != nil || mode != gwdns.ExitResolveReal || ip.String() != "203.0.113.10" {
		t.Fatalf("resolver = %v, %v, %v", ip, mode, err)
	}
	if gotDNS != "203.0.113.53:53" {
		t.Fatalf("warp lookup must use the in-tunnel DNS addr, got %q", gotDNS)
	}
}

func TestFakeDNSExitResolverTunnelDown(t *testing.T) {
	f := newFakeDNSTest(t)
	lookupCalled := false
	f.resolveExit = func(context.Context, string, string, int, string, string) (net.IP, error) {
		lookupCalled = true
		return nil, nil
	}
	f.SetExitHooks(nil, nil,
		func(string) int { return -1 },
		func(string) (string, bool) { return "10.7.0.1:18080", true },
		func(string) (string, bool) { return "", false })

	if _, _, err := f.exitResolver()(context.Background(), "api.example.com", "my-exit"); err == nil {
		t.Fatal("tunnel down must fail, not fall back to host DNS")
	}
	if lookupCalled {
		t.Fatal("tunnel down must not attempt the in-tunnel lookup")
	}
}

func TestFakeDNSExitResolverDirect(t *testing.T) {
	f := newFakeDNSTest(t)
	lookupCalled := false
	f.resolveExit = func(context.Context, string, string, int, string, string) (net.IP, error) {
		lookupCalled = true
		return nil, nil
	}
	f.SetExitHooks(nil, nil,
		func(string) int { return 5 },
		func(string) (string, bool) { return "10.7.0.1:18080", true },
		func(string) (string, bool) { return "", false })

	ip, mode, err := f.exitResolver()(context.Background(), "api.example.com", "")
	if err != nil || mode != gwdns.ExitResolveDirect || ip != nil {
		t.Fatalf("direct route = %v, %v, %v", ip, mode, err)
	}
	if lookupCalled {
		t.Fatal("direct route must use the normal upstream, not the exit lookup")
	}
}

func TestFakeDNSExitResolverLookupError(t *testing.T) {
	f := newFakeDNSTest(t)
	f.resolveExit = func(context.Context, string, string, int, string, string) (net.IP, error) {
		return nil, errors.New("exit refused name")
	}
	f.SetExitHooks(nil, nil,
		func(string) int { return 5 },
		func(string) (string, bool) { return "10.7.0.1:18080", true },
		func(string) (string, bool) { return "", false })

	if _, _, err := f.exitResolver()(context.Background(), "api.example.com", "my-exit"); err == nil {
		t.Fatal("lookup failure must propagate as SERVFAIL")
	}
}

func TestFakeDNSExitResolverNoTunnelResolver(t *testing.T) {
	f := newFakeDNSTest(t)
	f.SetExitHooks(nil, nil,
		func(string) int { return 5 },
		func(string) (string, bool) { return "", false },
		func(string) (string, bool) { return "", false })

	// production lookup path: no domain-connect and no in-tunnel DNS
	if _, _, err := f.exitResolver()(context.Background(), "api.example.com", "my-exit"); err == nil {
		t.Fatal("missing in-tunnel resolver must fail")
	}
}

func TestFakeDNSProbeAgentExitAnswersRealServerIP(t *testing.T) {
	f := newFakeDNSTest(t)
	lookupCalled := false
	f.resolveExit = func(context.Context, string, string, int, string, string) (net.IP, error) {
		lookupCalled = true
		return nil, nil
	}
	f.SetExitHooks(nil, nil,
		func(string) int { return 5 },
		func(string) (string, bool) { return "10.7.0.1:18080", true },
		func(string) (string, bool) { return "", false })

	ip, mode, err := f.exitResolver()(context.Background(), probeDomain, "my-exit")
	if err != nil || mode != gwdns.ExitResolveReal || ip.String() != "10.7.0.1" {
		t.Fatalf("probe on agent exit = %v, %v, %v", ip, mode, err)
	}
	if lookupCalled {
		t.Fatal("pvm.probe must never be resolved through a public/in-tunnel query")
	}
}

func TestFakeDNSProbeWarpKeepsFakeAnswer(t *testing.T) {
	f := newFakeDNSTest(t)
	f.SetExitHooks(nil, nil,
		func(string) int { return 6 },
		func(string) (string, bool) { return "", false },
		func(string) (string, bool) { return "203.0.113.53:53", true })

	ip, mode, err := f.exitResolver()(context.Background(), probeDomain, "my-warp")
	if err != nil || mode != gwdns.ExitResolveFake || ip != nil {
		t.Fatalf("probe on warp = %v, %v, %v", ip, mode, err)
	}
}

func TestFakeDNSProbeDirectKeepsFakeAnswer(t *testing.T) {
	f := newFakeDNSTest(t)
	f.SetExitHooks(nil, nil,
		func(string) int { return -1 },
		func(string) (string, bool) { return "", false },
		func(string) (string, bool) { return "", false })

	ip, mode, err := f.exitResolver()(context.Background(), probeDomain, "")
	if err != nil || mode != gwdns.ExitResolveFake || ip != nil {
		t.Fatalf("probe on direct = %v, %v, %v", ip, mode, err)
	}
}

func TestFakeDNSProbeTunnelDownFails(t *testing.T) {
	f := newFakeDNSTest(t)
	f.SetExitHooks(nil, nil,
		func(string) int { return -1 },
		func(string) (string, bool) { return "10.7.0.1:18080", true },
		func(string) (string, bool) { return "", false })

	if _, _, err := f.exitResolver()(context.Background(), probeDomain, "my-exit"); err == nil {
		t.Fatal("probe with the agent tunnel down must fail, not fake-fallback")
	}
}

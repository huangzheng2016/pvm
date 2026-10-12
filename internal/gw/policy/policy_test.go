package policy

import (
	"net"
	"testing"

	"github.com/huangzheng2016/pvm/internal/gw/dns"
	"github.com/huangzheng2016/pvm/internal/gw/rules"
)

func newTestEngine(t *testing.T) (*Engine, *rules.Engine, *dns.Pool) {
	t.Helper()
	eng, err := rules.NewEngine("", rules.DefaultInternal())
	if err != nil {
		t.Fatal(err)
	}
	pool, err := dns.NewPool("198.18.0.0/16", 1024)
	if err != nil {
		t.Fatal(err)
	}
	return New(eng, pool), eng, pool
}

func TestRuleProxyBeatsVMDefault(t *testing.T) {
	e, eng, _ := newTestEngine(t)
	e.SetVMExit("dev1", "jp")
	if err := eng.Add(rules.Rule{Type: rules.DomainSuffix, Pattern: "example.com", Action: rules.Proxy, Exit: "us"}); err != nil {
		t.Fatal(err)
	}
	d := e.DecideFlow("a.example.com", nil, "dev1")
	if d.Route != Proxy || d.Exit != "us" {
		t.Fatalf("rule exit must win: %+v", d)
	}
}

func TestDirectRuleBeatsVMDefault(t *testing.T) {
	e, eng, _ := newTestEngine(t)
	e.SetVMExit("dev1", "jp")
	if err := eng.Add(rules.Rule{Type: rules.DomainSuffix, Pattern: "corp.internal.example", Action: rules.Direct}); err != nil {
		t.Fatal(err)
	}
	if d := e.DecideFlow("a.corp.internal.example", nil, "dev1"); d.Route != Direct {
		t.Fatalf("direct rule must win over vm default: %+v", d)
	}
}

func TestInternalAlwaysDirect(t *testing.T) {
	e, _, _ := newTestEngine(t)
	e.SetVMExit("dev1", "jp")
	if d := e.DecideFlow("db.pvm.internal", nil, "dev1"); d.Route != Direct {
		t.Fatalf("internal must stay direct: %+v", d)
	}
}

func TestVMDefaultExit(t *testing.T) {
	e, _, _ := newTestEngine(t)
	if d := e.DecideFlow("api.anthropic.com", nil, "dev1"); d.Route != Direct {
		t.Fatalf("no exit configured should be direct: %+v", d)
	}
	e.SetVMExit("dev1", "jp")
	if d := e.DecideFlow("api.anthropic.com", nil, "dev1"); d.Route != Proxy || d.Exit != "jp" {
		t.Fatalf("vm default exit not applied: %+v", d)
	}
	if d := e.DecideFlow("api.anthropic.com", nil, "dev2"); d.Route != Direct {
		t.Fatalf("vm exit leaked to another vm: %+v", d)
	}
	// clearing restores direct
	e.SetVMExit("dev1", "")
	if d := e.DecideFlow("api.anthropic.com", nil, "dev1"); d.Route != Direct {
		t.Fatalf("cleared exit should be direct: %+v", d)
	}
}

func TestClusterDefaultExit(t *testing.T) {
	e, _, _ := newTestEngine(t)
	e.SetDefaultExit("us")
	e.SetVMExit("dev1", "jp")
	if d := e.DecideFlow("x.example.com", nil, "dev2"); d.Exit != "us" {
		t.Fatalf("cluster default not applied: %+v", d)
	}
	if d := e.DecideFlow("x.example.com", nil, "dev1"); d.Exit != "jp" {
		t.Fatalf("vm exit must beat cluster default: %+v", d)
	}
}

func TestCIDRRuleByIP(t *testing.T) {
	e, eng, _ := newTestEngine(t)
	if err := eng.Add(rules.Rule{Type: rules.CIDR, Pattern: "10.0.0.0/8", Action: rules.Direct}); err != nil {
		t.Fatal(err)
	}
	e.SetVMExit("dev1", "jp")
	if d := e.DecideFlow("", net.ParseIP("10.1.2.3"), "dev1"); d.Route != Direct {
		t.Fatalf("cidr direct must win over vm default: %+v", d)
	}
}

func TestFakeIPReverseResolution(t *testing.T) {
	e, _, pool := newTestEngine(t)
	e.SetVMExit("dev1", "jp")
	fake := pool.Allocate("api.anthropic.com")

	// a connection to a fake IP carries no name; policy must recover it
	if d := e.DecideFlow("", fake, "dev1"); d.Route != Proxy || d.Exit != "jp" {
		t.Fatalf("fake-ip flow should follow vm default: %+v", d)
	}

	domain, ok := e.LookupByFakeIP(fake)
	if !ok || domain != "api.anthropic.com" {
		t.Fatalf("LookupByFakeIP broken: %q ok=%v", domain, ok)
	}
	if !e.IsFakeIP(fake) {
		t.Fatal("IsFakeIP false negative")
	}
	if e.IsFakeIP(net.ParseIP("8.8.8.8")) {
		t.Fatal("IsFakeIP false positive")
	}
}

func TestFakeIPRuleMatchByDomain(t *testing.T) {
	e, eng, pool := newTestEngine(t)
	if err := eng.Add(rules.Rule{Type: rules.DomainSuffix, Pattern: "anthropic.com", Action: rules.Proxy, Exit: "us"}); err != nil {
		t.Fatal(err)
	}
	e.SetVMExit("dev1", "jp")
	fake := pool.Allocate("api.anthropic.com")

	// recovered domain feeds back into rules: us, not the vm default jp
	if d := e.DecideFlow("", fake, "dev1"); d.Exit != "us" {
		t.Fatalf("recovered domain should match rule exit: %+v", d)
	}
}

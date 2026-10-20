package gateway

import (
	"strings"
	"testing"
)

func stubIPRoute(t *testing.T) *ipRuleLog {
	t.Helper()
	orig := ipRoute
	log := &ipRuleLog{}
	ipRoute = log.record
	t.Cleanup(func() { ipRoute = orig })
	return log
}

func TestEnsureTunnelRoute(t *testing.T) {
	log := stubIPRule(t)
	routeLog := stubIPRoute(t)
	m := newExitManager()
	if err := m.ensureTunnelRoute(&exitTunnel{idx: 0, server: "203.0.113.1"}); err != nil {
		t.Fatal(err)
	}
	if !routeLog.has("replace", "default", "dev", "pvmwg0", "table", "100") {
		t.Fatalf("missing default route repair: %v", routeLog.calls)
	}
	if !log.has("add", "fwmark", "153", "lookup", "100", "pref", "100") {
		t.Fatalf("missing fwmark repair: %v", log.calls)
	}
	if !log.has("add", "to", "203.0.113.1/32", "lookup", "main", "pref", "40") {
		t.Fatalf("missing endpoint bypass repair: %v", log.calls)
	}
}

func TestCheckNFT(t *testing.T) {
	cfg := &Config{
		CIDR: "192.168.1.0/24", GwIP: "192.168.1.1",
		Exits: map[string]ExitDef{"my-exit": {Server: "127.0.0.1:1"}},
	}
	expected := RenderNFT(cfg, "enp0s1", "br0", nil)
	orig := nftListTable
	t.Cleanup(func() { nftListTable = orig })
	a := &Agent{upl: "enp0s1"}
	nftListTable = func() (string, error) { return expected, nil }
	if err := a.checkNFT(cfg); err != nil {
		t.Fatal(err)
	}
	nftListTable = func() (string, error) {
		return strings.Replace(expected, "redirect to :7894", "redirect to :9999", 1), nil
	}
	if err := a.checkNFT(cfg); err == nil || !strings.Contains(err.Error(), "missing rule") {
		t.Fatalf("missing UDP relay rule was not detected: %v", err)
	}
	nftListTable = func() (string, error) {
		return strings.Replace(expected, `oifname { "pvmwg0" } masquerade`, "", 1), nil
	}
	if err := a.checkNFT(cfg); err == nil {
		t.Fatal("missing tunnel NAT rule was not detected")
	}
}

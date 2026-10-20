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

func TestEnsureFakeIPRoute(t *testing.T) {
	ruleLog := stubIPRule(t)
	routeLog := stubIPRoute(t)
	if err := ensureFakeIPRoute(); err != nil {
		t.Fatal(err)
	}
	if !ruleLog.has("add", "fwmark", "80", "lookup", "80", "pref", "20") {
		t.Fatalf("missing fwmark rule: %v", ruleLog.calls)
	}
	if !routeLog.has("replace", "local", "default", "dev", "lo", "table", "80") {
		t.Fatalf("missing local route: %v", routeLog.calls)
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
	stubIPRule(t)
	stubIPRoute(t)
	a := &Agent{upl: "enp0s1"}
	nftListTable = func() (string, error) { return expected, nil }
	if err := a.checkNFT(cfg); err != nil {
		t.Fatal(err)
	}
	// nft normalizes output: single-element oifname sets are flattened and
	// marks are zero-padded. Neither may read as drift.
	nftListTable = func() (string, error) {
		out := strings.Replace(expected, `oifname { "pvmwg0" } masquerade`, `oifname "pvmwg0" masquerade`, 1)
		out = strings.ReplaceAll(out, "meta mark set 0x50", "meta mark set 0x00000050")
		out = strings.ReplaceAll(out, "meta mark != 0x51", "meta mark != 0x00000051")
		return out, nil
	}
	if err := a.checkNFT(cfg); err != nil {
		t.Fatalf("normalized nft output read as drift: %v", err)
	}
	nftListTable = func() (string, error) {
		return strings.Replace(expected, "tproxy ip to 192.168.1.1:7894", "tproxy ip to 192.168.1.1:9999", 1), nil
	}
	if err := a.checkNFT(cfg); err == nil || !strings.Contains(err.Error(), "missing rule") {
		t.Fatalf("changed UDP tproxy rule was not detected: %v", err)
	}
	nftListTable = func() (string, error) {
		return strings.Replace(expected, " meta mark set 0x50", "", 1), nil
	}
	if err := a.checkNFT(cfg); err == nil || !strings.Contains(err.Error(), "missing rule") {
		t.Fatalf("missing tproxy mark was not detected: %v", err)
	}
	nftListTable = func() (string, error) {
		return strings.Replace(expected, "icmp type echo-request meta mark set 0x50", "icmp type echo-request", 1), nil
	}
	if err := a.checkNFT(cfg); err == nil || !strings.Contains(err.Error(), "missing rule") {
		t.Fatalf("missing ICMP mark was not detected: %v", err)
	}
	nftListTable = func() (string, error) {
		return strings.Replace(expected, "icmp type echo-reply meta mark != 0x51 drop", "icmp type echo-reply drop", 1), nil
	}
	if err := a.checkNFT(cfg); err == nil || !strings.Contains(err.Error(), "missing rule") {
		t.Fatalf("changed echo-reply drop was not detected: %v", err)
	}
	nftListTable = func() (string, error) {
		return strings.Replace(expected, `oifname { "pvmwg0" } masquerade`, "", 1), nil
	}
	if err := a.checkNFT(cfg); err == nil {
		t.Fatal("missing tunnel NAT rule was not detected")
	}
}

func TestCheckNFTMultipleExits(t *testing.T) {
	cfg := &Config{
		CIDR: "192.168.1.0/24", GwIP: "192.168.1.1",
		Exits: map[string]ExitDef{
			"exit-a": {Server: "127.0.0.1:1"},
			"exit-b": {Server: "127.0.0.1:2"},
		},
	}
	expected := RenderNFT(cfg, "enp0s1", "br0", nil)
	orig := nftListTable
	t.Cleanup(func() { nftListTable = orig })
	stubIPRule(t)
	stubIPRoute(t)
	a := &Agent{upl: "enp0s1"}
	nftListTable = func() (string, error) { return expected, nil }
	if err := a.checkNFT(cfg); err != nil {
		t.Fatal(err)
	}
	nftListTable = func() (string, error) {
		return strings.Replace(expected, `"pvmwg0", `, "", 1), nil
	}
	if err := a.checkNFT(cfg); err == nil {
		t.Fatal("missing tunnel in a multi-exit NAT rule was not detected")
	}
}

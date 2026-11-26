package gateway

import (
	"net"
	"slices"
	"strings"
	"testing"

	gwdns "github.com/huangzheng2016/pvm/internal/gw/dns"
	gwpolicy "github.com/huangzheng2016/pvm/internal/gw/policy"
	"github.com/huangzheng2016/pvm/internal/gw/rules"
)

func TestCheckDirectTargetBlocksPrivateByDefault(t *testing.T) {
	eng, err := rules.NewEngine("", rules.DefaultInternal())
	if err != nil {
		t.Fatal(err)
	}
	pool, err := gwdns.NewPool("198.18.0.0/16", 0)
	if err != nil {
		t.Fatal(err)
	}
	pol := gwpolicy.New(eng, pool)

	for _, blocked := range []string{"10.0.2.2", "192.168.1.1", "172.16.0.1", "100.64.0.1", "169.254.169.254", "127.0.0.1", "224.0.0.1"} {
		if err := checkDirectTarget(pol, "lan.example.com", net.ParseIP(blocked)); err == nil {
			t.Errorf("private/reserved target %s was allowed", blocked)
		}
	}
	if err := checkDirectTarget(pol, "example.com", net.ParseIP("203.0.113.8")); err != nil {
		t.Fatalf("public target refused: %v", err)
	}
	if err := checkDirectTarget(pol, "dev1.pvm.internal", net.ParseIP("100.99.1.5")); err == nil {
		t.Fatal("built-in cluster internal suffix must not open uplink private access")
	}

	// Plain direct-domain rules resolving to private IPs stay blocked.
	if err := eng.Add(rules.Rule{Type: rules.DomainSuffix, Pattern: "example.com", Action: rules.Direct}); err != nil {
		t.Fatal(err)
	}
	if err := checkDirectTarget(pol, "lan.example.com", net.ParseIP("192.168.1.1")); err == nil {
		t.Fatal("direct domain rule must not implicitly allow private IPs")
	}

	// Explicit internal-domain rules and direct CIDR rules opt in.
	if err := eng.Add(rules.Rule{Type: rules.DomainSuffix, Pattern: "corp.test", Action: rules.Internal, DNS: "203.0.113.53"}); err != nil {
		t.Fatal(err)
	}
	if err := checkDirectTarget(pol, "git.corp.test", net.ParseIP("10.20.0.5")); err != nil {
		t.Fatalf("explicit internal rule refused: %v", err)
	}
	if err := eng.Add(rules.Rule{Type: rules.CIDR, Pattern: "192.168.50.0/24", Action: rules.Direct}); err != nil {
		t.Fatal(err)
	}
	if err := checkDirectTarget(pol, "", net.ParseIP("192.168.50.10")); err != nil {
		t.Fatalf("explicit direct CIDR refused: %v", err)
	}
}

func TestRenderNFTDirectPrivateIsolation(t *testing.T) {
	cfg := &Config{
		CIDR: "100.99.1.0/24", GwIP: "100.99.1.1",
		VMs: []VMEntry{{Name: "dev1", IP: "100.99.1.10", MAC: "10:65:30:01:01:0a"}},
	}
	out := RenderNFT(cfg, "enp0s1", "br0", []string{"203.0.113.0/24"}, []string{"192.168.50.0/24"})
	dnatReply := strings.Index(out, `ip daddr 10.0.2.0/24 ct direction reply ct status dnat accept`)
	privateAllow := strings.Index(out, `ip daddr @private_bypass4 accept`)
	privateDrop := strings.Index(out, `ip daddr @blocked_direct4 counter name pvm_drop_private drop`)
	bypass := strings.Index(out, `ip daddr @bypass4 accept`)
	directVM := strings.Index(out, `ip saddr 100.99.1.10 oifname "enp0s1" accept`)
	finalDrop := strings.LastIndex(out, `iifname "br0" oifname "enp0s1" drop`)
	if dnatReply < 0 || privateAllow < 0 || privateDrop < 0 || bypass < 0 || directVM < 0 || finalDrop < 0 {
		t.Fatalf("missing direct isolation rules:\n%s", out)
	}
	if !(dnatReply < privateAllow && privateAllow < privateDrop && privateDrop < bypass && bypass < directVM && directVM < finalDrop) {
		t.Fatalf("unexpected rule order:\n%s", out)
	}
	if !strings.Contains(out, "elements = { 192.168.50.0/24 }") || !strings.Contains(out, "100.64.0.0/10") {
		t.Fatalf("missing private set elements:\n%s", out)
	}
}

func TestExitManagerPrivateBypass(t *testing.T) {
	log := stubIPRule(t)
	m := newExitManager()
	var added, evicted []string
	m.onPrivateBypass = func(c string) { added = append(added, c) }
	m.onPrivateEvict = func(c string) { evicted = append(evicted, c) }

	// Plain direct-domain answers for private IPs are never learned.
	m.AddBypassIP("lan.example.com", "192.168.1.20", false)
	if m.bypass["192.168.1.20/32"] || m.privateBypass["192.168.1.20/32"] || log.has("add", "to 192.168.1.20/32") {
		t.Fatal("private direct-domain answer must not be learned")
	}

	// Explicit internal-rule answers and direct CIDRs are tracked and evicted.
	m.AddBypassIP("git.corp.example.com", "10.20.0.5", true)
	m.Sync(nil, nil, nil, nil, []string{"192.168.50.0/24"}, "100.99.1.0/24", "")
	if !slices.Contains(m.PrivateBypassList(), "10.20.0.5/32") || !slices.Contains(m.PrivateBypassList(), "192.168.50.0/24") {
		t.Fatalf("private bypass list = %v", m.PrivateBypassList())
	}
	if !slices.Contains(added, "10.20.0.5/32") || !slices.Contains(added, "192.168.50.0/24") {
		t.Fatalf("private bypass hooks = %v", added)
	}

	m.Sync(nil, nil, nil, nil, nil, "100.99.1.0/24", "")
	if slices.Contains(m.PrivateBypassList(), "192.168.50.0/24") || !slices.Contains(evicted, "192.168.50.0/24") {
		t.Fatalf("removed direct CIDR still in private bypass: list=%v evicted=%v", m.PrivateBypassList(), evicted)
	}
	if !slices.Contains(m.PrivateBypassList(), "10.20.0.5/32") {
		t.Fatal("learned internal bypass must survive Sync")
	}

	eng, err := rules.NewEngine("", rules.DefaultInternal())
	if err != nil {
		t.Fatal(err)
	}
	m.ReconcileLearned(eng)
	if len(m.PrivateBypassList()) != 0 {
		t.Fatalf("evicted internal IP still in private bypass: %v", m.PrivateBypassList())
	}
}

package gateway

import (
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huangzheng2016/pvm/internal/gw/rules"
	"github.com/huangzheng2016/pvm/internal/tunnel"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

// stubIPRule replaces the `ip rule` shell-out with a recorder.
func stubIPRule(t *testing.T) *ipRuleLog {
	t.Helper()
	log := &ipRuleLog{}
	orig := ipRuleCmd
	ipRuleCmd = log.record
	t.Cleanup(func() { ipRuleCmd = orig })
	return log
}

type ipRuleLog struct {
	mu    sync.Mutex
	calls [][]string
}

func (l *ipRuleLog) record(args ...string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, append([]string(nil), args...))
	return nil
}

func (l *ipRuleLog) has(parts ...string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.calls {
		joined := strings.Join(c, " ")
		match := true
		for _, p := range parts {
			if !strings.Contains(joined, p) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// Learned bypass IPs must survive config Syncs: dropping them mid-TTL
// reroutes cached direct flows into the tunnel.
func TestLearnedBypassSurvivesSync(t *testing.T) {
	log := stubIPRule(t)
	m := newExitManager()

	m.AddBypassIP("example.com", "203.0.113.9")
	if !log.has("add", "to 203.0.113.9/32", "main") {
		t.Fatalf("learn rule not installed: %v", log.calls)
	}

	m.Sync(nil, nil, nil, nil, nil, "", "")
	if log.has("del", "to 203.0.113.9/32") {
		t.Fatal("learned bypass was dropped by Sync")
	}
	if !m.bypass["203.0.113.9/32"] {
		t.Fatal("learned bypass missing from state after Sync")
	}
	// The slirp uplink subnet is always bypassed to main: exit-routed VMs
	// must be able to answer host port-map (DNAT) connections.
	if !log.has("add", "to 10.0.2.0/24", "main") {
		t.Fatalf("slirp bypass rule not installed: %v", log.calls)
	}
	if !m.bypass["10.0.2.0/24"] {
		t.Fatal("slirp bypass missing from state after Sync")
	}
}

func TestLearnedBypassCap(t *testing.T) {
	stubIPRule(t)
	m := newExitManager()
	m.learned["pad"] = map[string]time.Time{}
	for i := 0; i < learnedBypassCap; i++ {
		m.learned["pad"][strconv.Itoa(i)+".x"] = time.Now()
	}
	m.AddBypassIP("victim.com", "203.0.113.250")
	if m.bypass["203.0.113.250/32"] {
		t.Fatal("add beyond cap must be dropped")
	}
}

// Learned entries are evicted when their domain stops being a direct
// rule (e.g. flipped to proxy): ip rule, state and nft hook all go.
func TestReconcileLearnedEvictsFlippedRule(t *testing.T) {
	log := stubIPRule(t)
	m := newExitManager()
	var evicted []string
	m.onEvict = func(cidr string) { evicted = append(evicted, cidr) }

	eng, err := rules.NewEngine("", rules.DefaultInternal())
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Add(rules.Rule{Type: rules.DomainSuffix, Pattern: "example.com", Action: rules.Direct}); err != nil {
		t.Fatal(err)
	}
	m.AddBypassIP("www.example.com", "203.0.113.9")
	if !m.bypass["203.0.113.9/32"] {
		t.Fatal("learned ip missing")
	}

	// rule still direct: keep
	m.ReconcileLearned(eng)
	if !m.bypass["203.0.113.9/32"] {
		t.Fatal("direct-rule learned ip evicted too early")
	}

	// flip to proxy: evict
	eng2, err := rules.NewEngine("", rules.DefaultInternal())
	if err != nil {
		t.Fatal(err)
	}
	if err := eng2.Add(rules.Rule{Type: rules.DomainSuffix, Pattern: "example.com", Action: rules.Proxy, Exit: "my-exit"}); err != nil {
		t.Fatal(err)
	}
	m.ReconcileLearned(eng2)
	if m.bypass["203.0.113.9/32"] {
		t.Fatal("flipped-rule learned ip kept")
	}
	if !log.has("del", "to 203.0.113.9/32") {
		t.Fatal("ip rule not removed on evict")
	}
	if len(evicted) != 1 || evicted[0] != "203.0.113.9/32" {
		t.Fatalf("onEvict not fired: %v", evicted)
	}
}

// Learned entries expire after the TTL without any new sightings.
func TestSweepLearnedTTL(t *testing.T) {
	log := stubIPRule(t)
	m := newExitManager()
	m.learnedTTL = time.Hour
	var evicted []string
	m.onEvict = func(cidr string) { evicted = append(evicted, cidr) }

	m.AddBypassIP("example.com", "203.0.113.9")
	// pretend learned 2h ago while another is fresh
	m.learned["example.com"]["203.0.113.9/32"] = time.Now().Add(-2 * time.Hour)
	m.AddBypassIP("fresh.com", "203.0.113.10")

	m.SweepLearned(time.Now())
	if m.bypass["203.0.113.9/32"] {
		t.Fatal("stale learned ip kept past TTL")
	}
	if !m.bypass["203.0.113.10/32"] {
		t.Fatal("fresh learned ip evicted")
	}
	if !log.has("del", "to 203.0.113.9/32") {
		t.Fatal("stale ip rule not removed")
	}
	if len(evicted) != 1 {
		t.Fatalf("evictions: %v", evicted)
	}
}

// A repeated sighting refreshes the TTL instead of expiring.
func TestAddBypassIPRefreshesTTL(t *testing.T) {
	stubIPRule(t)
	m := newExitManager()
	m.learnedTTL = time.Hour
	m.AddBypassIP("example.com", "203.0.113.9")
	m.learned["example.com"]["203.0.113.9/32"] = time.Now().Add(-90 * time.Minute)
	m.AddBypassIP("example.com", "203.0.113.9")
	m.SweepLearned(time.Now())
	if !m.bypass["203.0.113.9/32"] {
		t.Fatal("refreshed entry expired")
	}
}

// Cluster default exit: whole-CIDR lookup at pref 103 with direct-VM
// overrides at pref 102; per-VM exits at 101 still win.
func TestSyncClusterDefaultExit(t *testing.T) {
	log := stubIPRule(t)
	m := newExitManager()
	exits := map[string]ExitDef{"my-exit": {Server: "127.0.0.1:1", Token: "x", Transport: "udp", Fingerprint: "00"}}
	vmIPs := map[string]string{"proxied": "192.168.1.10", "open": "192.168.1.11", "inherit": "192.168.1.12"}

	m.Sync(exits, map[string]string{"proxied": "my-exit", "open": "direct"}, vmIPs, nil, nil, "192.168.1.0/24", "my-exit")
	for _, want := range [][]string{
		{"add", "from 192.168.1.10/32", "pref", "101"},
		{"add", "from 192.168.1.11/32", "lookup", "main", "pref", "102"},
		{"add", "from 192.168.1.0/24", "lookup", "100", "pref", "103"},
	} {
		if !log.has(want...) {
			t.Fatalf("missing rule %v\n%v", want, log.calls)
		}
	}
	if log.has("add", "from 192.168.1.12/32", "lookup", "main", "pref", "102") {
		t.Fatal("unmatched VM must inherit the cluster exit instead of a main override")
	}

	// removing the default exit tears the cluster rule and overrides down
	log2 := stubIPRule(t)
	m.Sync(exits, map[string]string{"proxied": "my-exit"}, vmIPs, nil, nil, "", "")
	if !log2.has("del", "from 192.168.1.11/32") {
		t.Fatal("direct override not removed")
	}
	if !log2.has("del", "from 192.168.1.0/24") {
		t.Fatal("cluster rule not removed")
	}
	if m.cluster != "" || m.clusterTable != "" {
		t.Fatalf("cluster state not cleared: %+v", m.cluster)
	}
}

// ensureLocked must rebuild a tunnel whose index drifted after exit
// add/remove re-sorted the indexes, otherwise from-VM rules point at
// tables with no routes.
func TestEnsureRebuildsOnIndexDrift(t *testing.T) {
	stubIPRule(t)
	m := newExitManager()

	d := ExitDef{Server: "127.0.0.1:1", Token: "x", Transport: "udp", Fingerprint: "00"}

	done := make(chan struct{})
	close(done)
	old := &exitTunnel{name: "b", def: d, idx: 1, stop: make(chan struct{}), done: done}
	m.tuns["b"] = old

	m.ensureLocked("b", d, 0)
	cur := m.tuns["b"]
	if cur == old {
		t.Fatal("tunnel with drifted idx was kept")
	}
	if cur.idx != 0 {
		t.Fatalf("rebuilt tunnel idx = %d, want 0", cur.idx)
	}
	m.teardownLocked(cur, true)

	// same def + same idx keeps the existing tunnel
	done2 := make(chan struct{})
	close(done2)
	keep := &exitTunnel{name: "b", def: d, idx: 2, stop: make(chan struct{}), done: done2}
	m.tuns["b"] = keep
	m.ensureLocked("b", d, 2)
	if m.tuns["b"] != keep {
		t.Fatal("tunnel with matching def+idx must be kept")
	}
}

// The anti-loop host rule must be removed with the device.
func TestCleanupDeviceRemovesEndpointRule(t *testing.T) {
	log := stubIPRule(t)
	m := newExitManager()
	tt := &exitTunnel{name: "x", idx: 3, server: "198.51.100.7"}
	m.cleanupDevice(tt)
	if !log.has("del", "to 198.51.100.7/32") {
		t.Fatalf("endpoint rule not removed: %v", log.calls)
	}
}

// DomainConnectAddr must use the tunnel-side server address learned at
// enroll time; with a /16 pool it can no longer be derived from tunIP.
func TestDomainConnectAddrUsesEnrolledServerIP(t *testing.T) {
	td, _, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("10.100.0.7")}, nil, 1420)
	if err != nil {
		t.Fatal(err)
	}
	dev, err := tunnel.NewDevice(td, tunnel.BindFor("udp"), false)
	if err != nil {
		td.Close()
		t.Fatal(err)
	}
	defer dev.Close()

	m := newExitManager()
	m.tuns["my-exit"] = &exitTunnel{name: "my-exit", dev: dev, tunIP: "10.100.0.7", serverTunIP: "10.100.0.1"}
	addr, ok := m.DomainConnectAddr("my-exit")
	if !ok || addr != "10.100.0.1:18080" {
		t.Fatalf("DomainConnectAddr = %q, %v", addr, ok)
	}
	// a tunnel whose enroll returned no server ip must fail closed
	m.tuns["old"] = &exitTunnel{name: "old", dev: dev, tunIP: "10.99.0.7"}
	if _, ok := m.DomainConnectAddr("old"); ok {
		t.Fatal("missing serverTunIP must fail closed")
	}
	if _, ok := m.DomainConnectAddr("gone"); ok {
		t.Fatal("unknown exit must fail closed")
	}
}

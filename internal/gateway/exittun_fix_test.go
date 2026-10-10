package gateway

import (
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huangzheng2016/pvm/internal/gw/rules"
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

	// a config push with no direct CIDRs used to wipe the learned rule
	m.Sync(nil, nil, nil, nil, nil, "", "")
	if log.has("del", "to 203.0.113.9/32") {
		t.Fatal("learned bypass was dropped by Sync")
	}
	if !m.bypass["203.0.113.9/32"] {
		t.Fatal("learned bypass missing from state after Sync")
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
	if err := eng2.Add(rules.Rule{Type: rules.DomainSuffix, Pattern: "example.com", Action: rules.Proxy, Exit: "us"}); err != nil {
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
	exits := map[string]ExitDef{"us": {Server: "127.0.0.1:1", Token: "x", Transport: "udp", Fingerprint: "00"}}
	vmIPs := map[string]string{"proxied": "192.168.1.10", "open": "192.168.1.11"}

	m.Sync(exits, map[string]string{"proxied": "us"}, vmIPs, nil, nil, "192.168.1.0/24", "us")
	for _, want := range [][]string{
		{"add", "from 192.168.1.10/32", "pref", "101"},
		{"add", "from 192.168.1.11/32", "lookup", "main", "pref", "102"},
		{"add", "from 192.168.1.0/24", "lookup", "100", "pref", "103"},
	} {
		if !log.has(want...) {
			t.Fatalf("missing rule %v\n%v", want, log.calls)
		}
	}

	// removing the default exit tears the cluster rule and overrides down
	log2 := stubIPRule(t)
	m.Sync(exits, map[string]string{"proxied": "us"}, vmIPs, nil, nil, "", "")
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
	m.teardownLocked(cur)

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

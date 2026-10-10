package gateway

import (
	"strconv"
	"strings"
	"sync"
	"testing"
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

	m.AddBypassIP("203.0.113.9")
	if !log.has("add", "to 203.0.113.9/32", "main") {
		t.Fatalf("learn rule not installed: %v", log.calls)
	}

	// a config push with no direct CIDRs used to wipe the learned rule
	m.Sync(nil, nil, nil, nil, nil)
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
	for i := 0; i < learnedBypassCap; i++ {
		m.learned[strconv.Itoa(i)+".x"] = true
	}
	m.AddBypassIP("203.0.113.250")
	if m.learned["203.0.113.250/32"] {
		t.Fatal("add beyond cap must be dropped")
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

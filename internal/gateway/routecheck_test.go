package gateway

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"golang.zx2c4.com/wireguard/device"
)

// stubIPRuleList replaces the `ip -json rule show` reader with a fake.
func stubIPRuleList(t *testing.T, fn func() ([]ipRuleShowEntry, error)) {
	t.Helper()
	orig := ipRuleList
	ipRuleList = fn
	t.Cleanup(func() { ipRuleList = orig })
}

func ruleCalls(log *ipRuleLog) [][]string {
	log.mu.Lock()
	defer log.mu.Unlock()
	return append([][]string(nil), log.calls...)
}

func hasCall(log *ipRuleLog, want ...string) bool {
	for _, c := range ruleCalls(log) {
		if slices.Equal(c, want) {
			return true
		}
	}
	return false
}

// ruleDriftManager returns a manager whose in-memory state covers every
// desired rule kind, with one live tunnel (fake device, no goroutines).
func ruleDriftManager() *exitManager {
	m := newExitManager()
	m.vmRules["192.168.1.10"] = 0
	m.vmDirect["192.168.1.11"] = true
	m.cidrRules["203.0.113.0/24"] = 0
	m.bypass["10.0.2.0/24"] = true
	m.cluster = "192.168.1.0/24"
	m.clusterTable = "100"
	m.clusterLocal = "192.168.1.0/24"
	m.tuns["my-exit"] = &exitTunnel{name: "my-exit", idx: 0, server: "203.0.113.5", dev: &device.Device{}}
	return m
}

// actualRulesFor renders the manager's desired rules the way
// `ip -json rule show` would print them: host addresses without their
// /32, fwmark in hex, plus unrelated rules the reconciler must ignore.
func actualRulesFor(m *exitManager) []ipRuleShowEntry {
	out := []ipRuleShowEntry{
		{Priority: 0, Src: "all", Table: "local"},
		{Priority: 20, Src: "all", Fwmark: "0x50", Table: "80"},
		{Priority: 32766, Src: "all", Table: "main"},
	}
	for _, r := range m.desiredPolicyRulesLocked() {
		e := ipRuleShowEntry{Priority: r.pref, Src: "all", Table: r.table}
		for i := 0; i+1 < len(r.sel); i += 2 {
			switch r.sel[i] {
			case "from":
				e.Src = strings.TrimSuffix(r.sel[i+1], "/32")
			case "to":
				e.Dst = strings.TrimSuffix(r.sel[i+1], "/32")
			case "fwmark":
				e.Fwmark = normRuleFwmark(r.sel[i+1])
			}
		}
		out = append(out, e)
	}
	// iproute2 may omit src entirely for "all"; exercise the default.
	for i := range out {
		if out[i].Priority == 45 {
			out[i].Src = ""
		}
	}
	return out
}

// Missing desired rules of every kind are re-added; nothing is deleted.
func TestRuleDriftAddsMissingRules(t *testing.T) {
	log := stubIPRule(t)
	m := ruleDriftManager()
	stubIPRuleList(t, func() ([]ipRuleShowEntry, error) {
		return []ipRuleShowEntry{
			{Priority: 0, Src: "all", Table: "local"},
			{Priority: 32766, Src: "all", Table: "main"},
		}, nil
	})
	m.reconcileActualRulesLocked()
	for _, want := range [][]string{
		{"add", "to", "203.0.113.5/32", "lookup", "main", "pref", "40"},
		{"add", "to", "192.168.1.0/24", "lookup", "main", "pref", "45"},
		{"add", "to", "10.0.2.0/24", "lookup", "main", "pref", "50"},
		{"add", "to", "203.0.113.0/24", "lookup", "100", "pref", "60"},
		{"add", "fwmark", "153", "lookup", "100", "pref", "100"},
		{"add", "from", "192.168.1.10/32", "lookup", "100", "pref", "101"},
		{"add", "from", "192.168.1.11/32", "lookup", "main", "pref", "102"},
		{"add", "from", "192.168.1.0/24", "lookup", "100", "pref", "103"},
	} {
		if !hasCall(log, want...) {
			t.Fatalf("missing repair %v in %v", want, ruleCalls(log))
		}
	}
	for _, c := range ruleCalls(log) {
		if c[0] == "del" {
			t.Fatalf("unexpected delete %v", c)
		}
	}
}

// A rule whose selector+pref points at the wrong table is deleted by
// exact selector+pref and re-added with the desired table.
func TestRuleDriftFixesWrongTable(t *testing.T) {
	log := stubIPRule(t)
	m := ruleDriftManager()
	actual := actualRulesFor(m)
	for i := range actual {
		switch actual[i].Priority {
		case 101:
			actual[i].Table = "101" // exit index drifted
		case 50:
			actual[i].Table = "100" // bypass pointing into a tunnel table
		}
	}
	stubIPRuleList(t, func() ([]ipRuleShowEntry, error) { return actual, nil })
	m.reconcileActualRulesLocked()
	for _, want := range [][]string{
		{"del", "from", "192.168.1.10/32", "lookup", "101", "pref", "101"},
		{"del", "to", "10.0.2.0/24", "lookup", "100", "pref", "50"},
		{"add", "from", "192.168.1.10/32", "lookup", "100", "pref", "101"},
		{"add", "to", "10.0.2.0/24", "lookup", "main", "pref", "50"},
	} {
		if !hasCall(log, want...) {
			t.Fatalf("missing repair %v in %v", want, ruleCalls(log))
		}
	}
	// rules that were already correct must not be touched
	for _, c := range ruleCalls(log) {
		joined := strings.Join(c, " ")
		for _, pref := range []string{"pref 40", "pref 45", "pref 60", "pref 100", "pref 102", "pref 103"} {
			if strings.HasSuffix(joined, pref) {
				t.Fatalf("correct rule mutated: %v", c)
			}
		}
	}
}

// A fully correct rule set produces no mutation.
func TestRuleDriftCorrectRulesNoMutation(t *testing.T) {
	log := stubIPRule(t)
	m := ruleDriftManager()
	actual := actualRulesFor(m)
	stubIPRuleList(t, func() ([]ipRuleShowEntry, error) { return actual, nil })
	m.reconcileActualRulesLocked()
	if len(ruleCalls(log)) != 0 {
		t.Fatalf("correct rules mutated: %v", ruleCalls(log))
	}
}

// A failing reader must not trigger any mutation.
func TestRuleDriftReaderErrorNoMutation(t *testing.T) {
	log := stubIPRule(t)
	m := ruleDriftManager()
	stubIPRuleList(t, func() ([]ipRuleShowEntry, error) {
		return nil, errors.New("exec: ip: executable file not found")
	})
	m.reconcileActualRulesLocked()
	if len(ruleCalls(log)) != 0 {
		t.Fatalf("reader error caused mutation: %v", ruleCalls(log))
	}
}

// Sync runs the actual-rule check on every call, even when the desired
// state is unchanged: externally deleted rules (the production incident
// lost pref 45/50/103 while the config still had a cluster default) are
// restored without rebuilding any tunnel.
func TestSyncHealsExternallyDeletedRules(t *testing.T) {
	def := ExitDef{Server: "127.0.0.1:1", Token: "x", Transport: "udp", Fingerprint: "00"}
	exits := map[string]ExitDef{"my-exit": def}
	vmIPs := map[string]string{"vm1": "192.168.1.10"}
	vmExits := map[string]string{"vm1": "my-exit"}

	m := newExitManager()
	// a live tunnel with matching def/index: Sync never starts a real one
	m.tuns["my-exit"] = &exitTunnel{name: "my-exit", def: def, idx: 0, server: "203.0.113.5", dev: &device.Device{}}

	listCalls := 0
	var actual []ipRuleShowEntry
	stubIPRuleList(t, func() ([]ipRuleShowEntry, error) {
		listCalls++
		return append([]ipRuleShowEntry(nil), actual...), nil
	})
	log := stubIPRule(t)
	m.Sync(exits, vmExits, vmIPs, nil, nil, "192.168.1.0/24", "my-exit")
	if len(ruleCalls(log)) == 0 {
		t.Fatal("first Sync installed no rules")
	}

	// the kernel now holds the full rule set; externally delete the rules
	// lost in the production incident (pref 45, 50, 103)
	actual = actualRulesFor(m)
	kept := actual[:0]
	for _, e := range actual {
		if e.Priority == 45 || e.Priority == 50 || e.Priority == 103 {
			continue
		}
		kept = append(kept, e)
	}
	actual = kept

	log2 := stubIPRule(t)
	m.Sync(exits, vmExits, vmIPs, nil, nil, "192.168.1.0/24", "my-exit")
	if listCalls != 2 {
		t.Fatalf("ipRuleList called %d times, want once per Sync (2)", listCalls)
	}
	for _, want := range [][]string{
		{"add", "to", "192.168.1.0/24", "lookup", "main", "pref", "45"},
		{"add", "to", "10.0.2.0/24", "lookup", "main", "pref", "50"},
		{"add", "to", "203.0.113.5/32", "lookup", "main", "pref", "50"},
		{"add", "from", "192.168.1.0/24", "lookup", "100", "pref", "103"},
	} {
		if !hasCall(log2, want...) {
			t.Fatalf("missing repair %v in %v", want, ruleCalls(log2))
		}
	}
	// route-only drift must not disturb the tunnel
	if m.tuns["my-exit"].dev == nil {
		t.Fatal("tunnel device disturbed by rule repair")
	}
}

func TestRuleDriftAddsBeforeDeletingWrongTable(t *testing.T) {
	log := stubIPRule(t)
	m := ruleDriftManager()
	actual := actualRulesFor(m)
	for i := range actual {
		if actual[i].Priority == rulePrefClusterDefault {
			actual[i].Table = "main"
		}
	}
	stubIPRuleList(t, func() ([]ipRuleShowEntry, error) { return actual, nil })
	m.reconcileActualRulesLocked()
	add := []string{"add", "from", "192.168.1.0/24", "lookup", "100", "pref", "103"}
	del := []string{"del", "from", "192.168.1.0/24", "lookup", "main", "pref", "103"}
	addIndex, delIndex := -1, -1
	for i, c := range ruleCalls(log) {
		if slices.Equal(c, add) {
			addIndex = i
		}
		if slices.Equal(c, del) {
			delIndex = i
		}
	}
	if addIndex < 0 || delIndex < 0 || addIndex > delIndex {
		t.Fatalf("wrong-table repair is not add-first: add=%d del=%d calls=%v", addIndex, delIndex, ruleCalls(log))
	}
}

func TestRuleDriftAddFailureKeepsWrongTable(t *testing.T) {
	log := stubIPRule(t)
	orig := ipRuleCmd
	ipRuleCmd = func(args ...string) error {
		_ = log.record(args...)
		if args[0] == "add" {
			return errors.New("add failed")
		}
		return nil
	}
	t.Cleanup(func() { ipRuleCmd = orig })
	m := ruleDriftManager()
	actual := actualRulesFor(m)
	for i := range actual {
		if actual[i].Priority == rulePrefClusterDefault {
			actual[i].Table = "main"
		}
	}
	stubIPRuleList(t, func() ([]ipRuleShowEntry, error) { return actual, nil })
	m.reconcileActualRulesLocked()
	for _, c := range ruleCalls(log) {
		if c[0] == "del" {
			t.Fatalf("add failure deleted the old rule: %v", c)
		}
	}
}

func TestRuleDriftReplacesNonLookupRule(t *testing.T) {
	log := stubIPRule(t)
	m := ruleDriftManager()
	actual := actualRulesFor(m)
	for i := range actual {
		if actual[i].Priority == rulePrefClusterDefault {
			actual[i].Table = ""
		}
	}
	stubIPRuleList(t, func() ([]ipRuleShowEntry, error) { return actual, nil })
	m.reconcileActualRulesLocked()
	if !hasCall(log, "add", "from", "192.168.1.0/24", "lookup", "100", "pref", "103") {
		t.Fatalf("missing desired replacement: %v", ruleCalls(log))
	}
	if !hasCall(log, "del", "from", "192.168.1.0/24", "pref", "103") {
		t.Fatalf("non-lookup rule was not removed by exact selector+pref: %v", ruleCalls(log))
	}
}

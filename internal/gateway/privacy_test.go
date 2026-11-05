package gateway

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestSlirpOnlyPermitsMappedReplies(t *testing.T) {
	cfg := &Config{CIDR: "192.0.2.0/24", GwIP: "192.0.2.1", DefaultExit: "my-exit", Maps: []MapEntry{{HostPort: 8080, VMIP: "192.0.2.10", GuestPort: 80}}}
	out := RenderNFT(cfg, "eth0", "br0", []string{"10.0.2.0/24"})
	reply := strings.Index(out, "ip daddr 10.0.2.0/24 ct direction reply ct status dnat accept")
	drop := strings.Index(out, "ip daddr 10.0.2.0/24 drop")
	bypass := strings.Index(out, "ip daddr @bypass4 accept")
	if reply < 0 || drop <= reply || bypass <= drop {
		t.Fatalf("slirp protection order incorrect: %s", out)
	}
	if !strings.Contains(out, `iifname "eth0" tcp dport 8080 dnat ip to 192.0.2.10:80`) {
		t.Fatal("port mapping lost")
	}
	if strings.Contains(out, "ip daddr 10.0.2.0/24 accept") {
		t.Fatal("unrestricted host access")
	}
}

func TestEndpointIsNotGuestBypass(t *testing.T) {
	stubIPRule(t)
	m := ruleDriftManager()
	actual := actualRulesFor(m)
	stubIPRuleList(t, func() ([]ipRuleShowEntry, error) { return actual, nil })
	exits := map[string]ExitDef{"my-exit": m.tuns["my-exit"].def}
	m.Sync(exits, nil, nil, nil, nil, "192.168.1.0/24", "my-exit")
	if m.bypass["203.0.113.5/32"] {
		t.Fatal("endpoint exposed to guest bypass")
	}
	for _, r := range m.desiredPolicyRulesLocked() {
		if r.pref != rulePrefEndpoint {
			continue
		}
		if r.iif != "lo" || r.fwmark != "0x0" {
			t.Fatalf("unscoped endpoint rule: %+v", r)
		}
		e := ipRuleShowEntry{Priority: r.pref, Src: "all", Dst: r.dst, Fwmark: "0x0", IIF: "br0"}
		if r.matchesSelector(e) {
			t.Fatal("forwarded rule matches local endpoint exception")
		}
	}
}

func TestDumpMappingsPrivateAtomic(t *testing.T) {
	f := newFakeDNSTest(t)
	ip := f.pool.Allocate("example.com")
	path := filepath.Join(t.TempDir(), "private", "fakeip.json")
	if err := f.DumpMappings(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if err := f.DumpMappings(path); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0600 {
		t.Fatalf("mode = %o", st.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["example.com"] != ip.String() {
		t.Fatalf("unexpected mapping: %v", m)
	}
	if err := f.DumpMappings(filepath.Join(path, "invalid")); err == nil {
		t.Fatal("write error was hidden")
	}
}

type quotaConn struct{ net.Conn }

func (*quotaConn) Close() error { return nil }

func TestFakeProxyAdmissionLimitsAndRelease(t *testing.T) {
	for _, tc := range []struct {
		name   string
		count  int
		active bool
		vm     func(int) string
	}{
		{"pending per VM", fakeProxyMaxPendingPerVM, false, func(int) string { return "dev1" }},
		{"pending total", fakeProxyMaxPending, false, func(i int) string { return fmt.Sprint(i) }},
		{"active per VM", fakeProxyMaxPerVM, true, func(int) string { return "dev1" }},
		{"active total", fakeProxyMaxConnections, true, func(i int) string { return fmt.Sprint(i) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newFakeProxy(nil, "192.0.2.1", nil)
			var held []net.Conn
			for i := 0; i < tc.count; i++ {
				c := &quotaConn{}
				if !p.admit(c, tc.vm(i)) {
					t.Fatalf("rejected at %d", i)
				}
				held = append(held, c)
				if tc.active {
					p.mu.Lock()
					p.connectedLocked(c)
					p.mu.Unlock()
				}
			}
			if p.admit(&quotaConn{}, tc.vm(tc.count)) {
				t.Fatal("quota exceeded")
			}
			p.release(held[0])
			next := &quotaConn{}
			if !p.admit(next, tc.vm(0)) {
				t.Fatal("released quota not reusable")
			}
			p.release(next)
			for _, c := range held {
				p.release(c)
			}
			if len(p.admissions) != 0 || len(p.perVM) != 0 || len(p.pendingPerVM) != 0 || p.pending != 0 {
				t.Fatal("quota leak")
			}
			p.Close()
			if p.admit(&quotaConn{}, "dev1") {
				t.Fatal("admitted after close")
			}
		})
	}
}

func TestEndpointReconcileRemovesOnlyUnscopedOwnedRules(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			log := stubIPRule(t)
			m := ruleDriftManager()
			if explicit {
				m.bypass["203.0.113.5/32"] = true
			}
			actual := actualRulesFor(m)
			actual = append(actual, ipRuleShowEntry{Priority: rulePrefEndpoint, Src: "all", Dst: "203.0.113.5", Table: "main"})
			if !explicit {
				actual = append(actual, ipRuleShowEntry{Priority: rulePrefBypass, Src: "all", Dst: "203.0.113.5", Table: "main"})
			}
			actual = append(actual, ipRuleShowEntry{Priority: rulePrefEndpoint, Src: "all", Dst: "203.0.113.7", Table: "main"})
			stubIPRuleList(t, func() ([]ipRuleShowEntry, error) { return actual, nil })
			m.reconcileActualRulesLocked()
			if !hasCall(log, "del", "to", "203.0.113.5/32", "fwmark", "0/0", "lookup", "main", "pref", "40") {
				t.Fatal("old endpoint rule retained")
			}
			removed := hasCall(log, "del", "to", "203.0.113.5/32", "fwmark", "0/0", "lookup", "main", "pref", "50")
			if removed == explicit {
				t.Fatal("explicit bypass handling incorrect")
			}
			for _, c := range ruleCalls(log) {
				if strings.Contains(strings.Join(c, " "), "203.0.113.7") || c[0] == "add" {
					t.Fatalf("unrelated or correct rule touched: %v", c)
				}
			}
		})
	}
}

func TestFakeProxyAdmissionConcurrent(t *testing.T) {
	p := newFakeProxy(nil, "192.0.2.1", nil)
	const clients = 128
	results := make(chan bool, clients)
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := &quotaConn{}
			ok := p.admit(c, "dev1")
			results <- ok
			if ok {
				<-release
				p.release(c)
			}
		}()
	}
	admitted := 0
	for i := 0; i < clients; i++ {
		if <-results {
			admitted++
		}
	}
	close(release)
	wg.Wait()
	if admitted != fakeProxyMaxPendingPerVM {
		t.Fatalf("admitted %d; want %d", admitted, fakeProxyMaxPendingPerVM)
	}
	if len(p.admissions) != 0 || len(p.perVM) != 0 || len(p.pendingPerVM) != 0 || p.pending != 0 {
		t.Fatal("concurrent admission leaked quota")
	}
}

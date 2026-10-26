package rules

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := NewEngine("", DefaultInternal())
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func mustAdd(t *testing.T, e *Engine, r Rule) {
	t.Helper()
	if err := e.Add(r); err != nil {
		t.Fatal(err)
	}
}

func TestInternalSuffixAlwaysDirect(t *testing.T) {
	e := newTestEngine(t)
	mustAdd(t, e, Rule{Type: DomainSuffix, Pattern: "internal", Action: Proxy, Exit: "my-exit"})

	d, ok := e.Match("host.pvm.internal", nil)
	if !ok || !d.Internal || d.Action != Direct {
		t.Fatalf("internal suffix must stay direct: %+v ok=%v", d, ok)
	}
}

func TestDomainRuleClasses(t *testing.T) {
	e := newTestEngine(t)
	mustAdd(t, e, Rule{Type: DomainSuffix, Pattern: "corp.example.com", Action: Direct})
	mustAdd(t, e, Rule{Type: DomainPrefix, Pattern: "intranet.", Action: Direct})
	mustAdd(t, e, Rule{Type: DomainKeyword, Pattern: "gitlab", Action: Proxy, Exit: "my-exit"})

	cases := []struct {
		domain string
		action Action
		exit   string
	}{
		{"corp.example.com", Direct, ""},
		{"git.corp.example.com", Direct, ""},
		{"intranet.example.org", Direct, ""},
		{"my-gitlab.example.org", Proxy, "my-exit"},
	}
	for _, c := range cases {
		d, ok := e.Match(c.domain, nil)
		if !ok || d.Action != c.action || d.Exit != c.exit {
			t.Fatalf("%s: got %+v ok=%v", c.domain, d, ok)
		}
	}
}

func TestDomainRulesBeatCIDR(t *testing.T) {
	e := newTestEngine(t)
	mustAdd(t, e, Rule{Type: CIDR, Pattern: "10.0.0.0/8", Action: Direct})
	mustAdd(t, e, Rule{Type: DomainSuffix, Pattern: "example.com", Action: Proxy, Exit: "jp"})

	d, ok := e.Match("a.example.com", net.ParseIP("10.1.2.3"))
	if !ok || d.Action != Proxy || d.Exit != "jp" {
		t.Fatalf("domain rule must win over CIDR: %+v ok=%v", d, ok)
	}
}

func TestCIDRRule(t *testing.T) {
	e := newTestEngine(t)
	mustAdd(t, e, Rule{Type: CIDR, Pattern: "10.0.0.0/8", Action: Direct})

	if _, ok := e.Match("", net.ParseIP("10.9.9.9")); !ok {
		t.Fatal("cidr should match")
	}
	if _, ok := e.Match("", net.ParseIP("11.0.0.1")); ok {
		t.Fatal("cidr should not match")
	}
}

func TestFirstMatchWins(t *testing.T) {
	e := newTestEngine(t)
	mustAdd(t, e, Rule{Type: DomainSuffix, Pattern: "example.com", Action: Direct})
	mustAdd(t, e, Rule{Type: DomainSuffix, Pattern: "sub.example.com", Action: Proxy, Exit: "my-exit"})

	d, _ := e.Match("sub.example.com", nil)
	if d.Action != Direct {
		t.Fatalf("insertion order must win: %+v", d)
	}
}

func TestHotAddRemove(t *testing.T) {
	e := newTestEngine(t)
	if _, ok := e.Match("x.example.com", nil); ok {
		t.Fatal("unexpected match before add")
	}
	mustAdd(t, e, Rule{Type: DomainSuffix, Pattern: "example.com", Action: Direct})
	if _, ok := e.Match("x.example.com", nil); !ok {
		t.Fatal("rule not effective immediately after add")
	}
	ok, err := e.Remove(0)
	if !ok || err != nil {
		t.Fatal(err)
	}
	if _, ok := e.Match("x.example.com", nil); ok {
		t.Fatal("rule still effective after remove")
	}
}

func TestReplaceSamePattern(t *testing.T) {
	e := newTestEngine(t)
	mustAdd(t, e, Rule{Type: DomainSuffix, Pattern: "example.com", Action: Direct})
	mustAdd(t, e, Rule{Type: DomainSuffix, Pattern: "example.com", Action: Proxy, Exit: "my-exit"})
	if got := len(e.List()); got != 1 {
		t.Fatalf("replace should not duplicate: %d", got)
	}
	d, _ := e.Match("example.com", nil)
	if d.Action != Proxy || d.Exit != "my-exit" {
		t.Fatalf("action not replaced: %+v", d)
	}
}

func TestPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.json")
	e, err := NewEngine(path, DefaultInternal())
	if err != nil {
		t.Fatal(err)
	}
	mustAdd(t, e, Rule{Type: DomainSuffix, Pattern: "example.com", Action: Proxy, Exit: "my-exit"})
	mustAdd(t, e, Rule{Type: CIDR, Pattern: "10.0.0.0/8", Action: Direct})

	e2, err := NewEngine(path, DefaultInternal())
	if err != nil {
		t.Fatal(err)
	}
	if got := len(e2.List()); got != 2 {
		t.Fatalf("rules not persisted: %d", got)
	}
	d, ok := e2.Match("a.example.com", nil)
	if !ok || d.Action != Proxy || d.Exit != "my-exit" {
		t.Fatalf("reloaded match wrong: %+v ok=%v", d, ok)
	}
	if _, ok := e2.Match("", net.ParseIP("10.1.1.1")); !ok {
		t.Fatal("reloaded cidr not matching")
	}
}

func TestNormalization(t *testing.T) {
	e := newTestEngine(t)
	mustAdd(t, e, Rule{Type: DomainSuffix, Pattern: ".Example.COM.", Action: Direct})
	if _, ok := e.Match("WWW.EXAMPLE.COM.", nil); !ok {
		t.Fatal("case/trailing-dot normalization broken")
	}
}

func TestFailedPersistenceKeepsActiveRules(t *testing.T) {
	e := newTestEngine(t)
	r := Rule{Type: DomainSuffix, Pattern: "example.com", Action: Direct}
	mustAdd(t, e, r)
	// An existing directory cannot be replaced by the rules file.
	e.path = t.TempDir()
	if err := e.Add(Rule{Type: DomainSuffix, Pattern: "example.com", Action: Proxy, Exit: "my-exit"}); err == nil {
		t.Fatal("expected write failure")
	}
	if d, _ := e.Match("example.com", nil); d.Action != Direct {
		t.Fatal("failed replacement changed live routing")
	}
	if err := e.Add(Rule{Type: DomainSuffix, Pattern: "new.example.com", Action: Direct}); err == nil {
		t.Fatal("expected write failure")
	}
	if len(e.List()) != 1 {
		t.Fatal("failed add changed live rules")
	}
	if ok, err := e.Remove(0); err == nil || ok {
		t.Fatal("expected failed removal")
	}
	if len(e.List()) != 1 {
		t.Fatal("failed removal changed live rules")
	}
}

func TestRejectInvalidRulesOnAddAndLoad(t *testing.T) {
	for _, r := range []Rule{
		{Type: "unknown", Pattern: "example.com", Action: Direct},
		{Type: DomainSuffix, Pattern: "example.com", Action: "unknown"},
		{Type: DomainSuffix, Pattern: " . ", Action: Direct},
		{Type: DomainPrefix, Pattern: "", Action: Direct},
		{Type: DomainKeyword, Pattern: "", Action: Direct},
	} {
		e := newTestEngine(t)
		if err := e.Add(r); err == nil {
			t.Fatalf("Add accepted invalid rule: %+v", r)
		}
		data, err := json.Marshal([]Rule{r})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "rules.json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewEngine(path, nil); err == nil {
			t.Fatalf("Load accepted invalid rule: %+v", r)
		}
	}
}

// Package rules implements the gateway split-routing rule engine.
//
// Match priority: internal suffixes > user domain rules > user CIDR
// rules > caller's default. User rules are first-match-wins in
// insertion order within their class.
package rules

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/huangzheng2016/pvm/internal/fsutil"
)

type Type string

const (
	DomainSuffix  Type = "domain-suffix"
	DomainPrefix  Type = "domain-prefix"
	DomainKeyword Type = "domain-keyword"
	CIDR          Type = "cidr"
)

type Action string

const (
	Direct Action = "direct"
	Proxy  Action = "proxy"
	// Internal resolves the name only via the rule's designated internal
	// DNS server; the traffic itself goes direct (never through a tunnel).
	Internal Action = "internal"
)

type Rule struct {
	Type    Type   `json:"type"`
	Pattern string `json:"pattern"`
	Action  Action `json:"action"`
	Exit    string `json:"exit,omitempty"`
	DNS     string `json:"dns,omitempty"` // internal action: designated resolver host[:port]
}

type Decision struct {
	Action   Action
	Exit     string
	Internal bool   // matched a built-in internal suffix, never leaves the cluster
	DNS      string // Internal action: designated resolver
}

type entry struct {
	Rule
	cidr *net.IPNet
}

type Engine struct {
	mu       sync.RWMutex
	entries  []entry
	internal []string
	path     string
}

func DefaultInternal() []string {
	return []string{"pvm.internal", "internal"}
}

func NewEngine(path string, internalSuffixes []string) (*Engine, error) {
	e := &Engine{path: path, internal: normalizeList(internalSuffixes)}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return e, nil
	}
	if err != nil {
		return nil, err
	}
	var rules []Rule
	if err := json.Unmarshal(data, &rules); err != nil {
		return nil, err
	}
	for _, r := range rules {
		en, err := compile(r)
		if err != nil {
			return nil, err
		}
		e.entries = append(e.entries, en)
	}
	return e, nil
}

func normalizeList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if n := normalizeDomain(s); n != "" {
			out = append(out, n)
		}
	}
	return out
}

func normalizeDomain(d string) string {
	d = strings.ToLower(strings.TrimSpace(d))
	d = strings.TrimPrefix(d, ".")
	return strings.TrimSuffix(d, ".")
}

func compile(r Rule) (entry, error) {
	en := entry{Rule: r}
	switch r.Type {
	case DomainSuffix, DomainPrefix, DomainKeyword, CIDR:
	default:
		return en, fmt.Errorf("invalid rule type %q", r.Type)
	}
	switch r.Action {
	case Direct, Proxy, Internal:
	default:
		return en, fmt.Errorf("invalid rule action %q", r.Action)
	}
	if normalizeDomain(r.Pattern) == "" {
		return en, fmt.Errorf("rule pattern must not be empty")
	}
	if r.Type == CIDR {
		_, n, err := net.ParseCIDR(r.Pattern)
		if err != nil {
			return en, err
		}
		en.cidr = n
	} else {
		en.Pattern = normalizeDomain(r.Pattern)
	}
	return en, nil
}

func (e *Engine) IsInternal(domain string) bool {
	domain = normalizeDomain(domain)
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, s := range e.internal {
		if domain == s || strings.HasSuffix(domain, "."+s) {
			return true
		}
	}
	return false
}

func (e *Engine) Match(domain string, ip net.IP) (Decision, bool) {
	domain = normalizeDomain(domain)
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, s := range e.internal {
		if domain == s || strings.HasSuffix(domain, "."+s) {
			return Decision{Action: Direct, Internal: true}, true
		}
	}
	if domain != "" {
		for _, en := range e.entries {
			if en.Type == CIDR {
				continue
			}
			if matchDomain(en.Type, en.Pattern, domain) {
				return Decision{Action: en.Action, Exit: en.Exit, DNS: en.DNS}, true
			}
		}
	}
	if ip != nil {
		for _, en := range e.entries {
			if en.cidr != nil && en.cidr.Contains(ip) {
				return Decision{Action: en.Action, Exit: en.Exit}, true
			}
		}
	}
	return Decision{}, false
}

func matchDomain(t Type, pattern, domain string) bool {
	switch t {
	case DomainSuffix:
		return domain == pattern || strings.HasSuffix(domain, "."+pattern)
	case DomainPrefix:
		return strings.HasPrefix(domain, pattern)
	case DomainKeyword:
		return strings.Contains(domain, pattern)
	}
	return false
}

func (e *Engine) List() []Rule {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]Rule, len(e.entries))
	for i, en := range e.entries {
		out[i] = en.Rule
	}
	return out
}

func (e *Engine) Add(r Rule) error {
	en, err := compile(r)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, ex := range e.entries {
		if ex.Type == en.Type && ex.Pattern == en.Pattern {
			e.entries[i] = en
			if err := e.saveLocked(); err != nil {
				e.entries[i] = ex
				return err
			}
			return nil
		}
	}
	e.entries = append(e.entries, en)
	if err := e.saveLocked(); err != nil {
		e.entries = e.entries[:len(e.entries)-1]
		return err
	}
	return nil
}

func (e *Engine) Remove(index int) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if index < 0 || index >= len(e.entries) {
		return false, nil
	}
	before := e.entries
	e.entries = append(append([]entry(nil), before[:index]...), before[index+1:]...)
	if err := e.saveLocked(); err != nil {
		e.entries = before
		return false, err
	}
	return true, nil
}

func (e *Engine) saveLocked() error {
	if e.path == "" {
		return nil
	}
	rules := make([]Rule, len(e.entries))
	for i, en := range e.entries {
		rules[i] = en.Rule
	}
	data, err := json.MarshalIndent(rules, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWrite(e.path, data, 0o600)
}

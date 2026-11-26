package gateway

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/huangzheng2016/pvm/internal/fsutil"
)

const grantsPath = "/run/pvm/grants.json"

// GrantLease atomically records a verification lease for vmName. The running
// gateway agent polls this root-owned file and expires leases locally even if
// the host daemon disappears.
func GrantLease(c *Config, vmName string, seconds int) error {
	if seconds < 1 || seconds > 300 {
		return fmt.Errorf("lease seconds out of range [1,300]")
	}
	found := false
	for _, v := range c.VMs {
		if v.Name == vmName {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("vm %q not in gateway config", vmName)
	}
	grants := map[string]time.Time{}
	if raw, err := os.ReadFile(grantsPath); err == nil {
		_ = json.Unmarshal(raw, &grants)
	}
	grants[vmName] = time.Now().Add(time.Duration(seconds) * time.Second)
	data, err := json.Marshal(grants)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(grantsPath), 0o700); err != nil {
		return err
	}
	return fsutil.AtomicWrite(grantsPath, data, 0o600)
}

type admissionState struct {
	mu       sync.Mutex
	lastRaw  []byte
	grants   map[string]time.Time
	admitted map[string]bool // VM name -> currently allowed past unverified4
}

func (s *admissionState) reconcile(c *Config) {
	now := time.Now()
	if raw, err := os.ReadFile(grantsPath); err == nil && string(raw) != string(s.lastRaw) {
		var g map[string]time.Time
		if json.Unmarshal(raw, &g) == nil {
			s.grants = g
			s.lastRaw = raw
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.admitted == nil {
		s.admitted = map[string]bool{}
	}
	for _, v := range c.VMs {
		want := !c.RequireCheck[v.Name] || now.Before(s.grants[v.Name])
		cur, known := s.admitted[v.Name]
		if known && cur == want {
			continue
		}
		s.admitted[v.Name] = want
		set, verb := "verified_vms", "add"
		if !want {
			set, verb = "unverified4", "add"
		}
		other, otherVerb := "unverified4", "delete"
		if !want {
			other, otherVerb = "verified_vms", "delete"
		}
		_ = exec.Command("nft", otherVerb, "element", "inet", "pvm", other, "{", v.IP, "}").Run()
		if out, err := exec.Command("nft", verb, "element", "inet", "pvm", set, "{", v.IP, "}").CombinedOutput(); err != nil {
			log.Printf("admission %s %s (%s): %v (%s)", verb, v.Name, set, err, out)
			delete(s.admitted, v.Name)
			continue
		}
		if c.RequireCheck[v.Name] {
			// Cut established external connections immediately on revoke so
			// long-lived flows cannot outlive their lease.
			_ = exec.Command("conntrack", "-D", "-s", v.IP).Run()
			log.Printf("admission: %s verified=%v", v.Name, want)
		}
	}
}

// AdmittedVMIPs renders the current admission sets for full nft rebuilds.
func (s *admissionState) sets(c *Config) [2][]string {
	var verified, unverified []string
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range c.VMs {
		if !c.RequireCheck[v.Name] || now.Before(s.grants[v.Name]) {
			verified = append(verified, v.IP)
		} else {
			unverified = append(unverified, v.IP)
		}
	}
	sort.Strings(verified)
	sort.Strings(unverified)
	return [2][]string{verified, unverified}
}

var _ = strconv.Itoa

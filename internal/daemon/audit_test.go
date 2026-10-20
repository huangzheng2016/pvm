package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/huangzheng2016/pvm/internal/cluster"
	"github.com/huangzheng2016/pvm/internal/exit"
	"github.com/huangzheng2016/pvm/internal/paths"
	"github.com/huangzheng2016/pvm/internal/qemu"
	"github.com/huangzheng2016/pvm/internal/vm"
)

func newAuditTestServer(t *testing.T, spec *vm.Spec) *Server {
	t.Helper()
	t.Setenv("PVM_HOME", t.TempDir())
	p, err := paths.Default()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.VMDir(spec.Name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := spec.Save(p); err != nil {
		t.Fatal(err)
	}
	st, err := cluster.LoadStore(filepath.Join(p.Home, "clusters"))
	if err != nil {
		t.Fatal(err)
	}
	return &Server{P: p, Clusters: st, vms: map[string]*vm.Spec{spec.Name: spec}}
}

// markAuditVMRunning writes a run.json whose process identity is the test
// process itself, so Running() sees a live VM without any QEMU.
func markAuditVMRunning(t *testing.T, s *Server, spec *vm.Spec) {
	t.Helper()
	ident, err := qemu.IdentityOf(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(qemu.RunState{PID: os.Getpid(), Ident: ident, Started: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.P.VMDir(spec.Name), "run.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

type fakeGuestRunner struct {
	t    *testing.T
	cmds []string
	fn   func(cmd string) (string, error)
}

func (f *fakeGuestRunner) run(_ *vm.Spec, cmd string) (string, error) {
	f.cmds = append(f.cmds, cmd)
	return f.fn(cmd)
}

func decodeAuditChecks(t *testing.T, body []byte) ([]auditCheck, map[string]auditCheck) {
	t.Helper()
	var list []auditCheck
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	byName := map[string]auditCheck{}
	for _, c := range list {
		byName[c.Name] = c
	}
	return list, byName
}

// auditRequest builds a direct auditVM request; SetPathValue stands in
// for the mux pattern vars that ServeHTTP would populate.
func auditRequest() *http.Request {
	req := httptest.NewRequest("GET", "/v1/vms/dev1/audit", nil)
	req.SetPathValue("name", "dev1")
	return req
}

func requireAuditStatus(t *testing.T, checks map[string]auditCheck, name, status string) auditCheck {
	t.Helper()
	c, ok := checks[name]
	if !ok {
		t.Fatalf("check %q missing in %+v", name, checks)
	}
	if c.Status != status {
		t.Fatalf("check %s = %s: %s, want %s", name, c.Status, c.Detail, status)
	}
	return c
}

func TestAuditStoppedVM(t *testing.T) {
	spec := &vm.Spec{Name: "dev1", Role: "user", User: "dev", CPUs: 1, MemMB: 128, ClusterID: 1, IP: "100.99.1.10", MAC: "10:65:30:01:00:0a"}
	s := newAuditTestServer(t, spec)
	specBefore, err := os.ReadFile(spec.SpecPath(s.P))
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest("GET", "/v1/vms/dev1/audit", nil))
	if rr.Code != 200 {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	list, checks := decodeAuditChecks(t, rr.Body.Bytes())
	wantOrder := []string{"vm", "route", "udp", "icmp", "dns", "ipv6", "timezone", "locale"}
	if len(list) != len(wantOrder) {
		t.Fatalf("got %d checks, want %d: %+v", len(list), len(wantOrder), list)
	}
	for i, name := range wantOrder {
		if list[i].Name != name {
			t.Fatalf("check %d = %s, want %s", i, list[i].Name, name)
		}
	}
	requireAuditStatus(t, checks, "vm", "warn")
	if checks["vm"].Detail != "stopped" {
		t.Fatalf("vm detail = %q", checks["vm"].Detail)
	}
	for _, name := range wantOrder[1:] {
		c := requireAuditStatus(t, checks, name, "skipped")
		if c.Detail != "vm not running" {
			t.Fatalf("%s detail = %q", name, c.Detail)
		}
	}

	// Read-only: the spec is untouched and no run state was invented.
	specAfter, err := os.ReadFile(spec.SpecPath(s.P))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(specBefore, specAfter) {
		t.Fatal("audit modified the vm spec")
	}
	if _, err := os.Stat(filepath.Join(s.P.VMDir(spec.Name), "run.json")); !os.IsNotExist(err) {
		t.Fatal("audit created run.json for a stopped vm")
	}
	if _, err := os.Stat(filepath.Join(s.P.Home, "clusters", "clusters.json")); !os.IsNotExist(err) {
		t.Fatal("audit persisted cluster state")
	}
}

func TestAuditUnknownVM(t *testing.T) {
	s := newAuditTestServer(t, &vm.Spec{Name: "dev1", Role: "user", User: "dev", CPUs: 1, MemMB: 128})
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest("GET", "/v1/vms/nope/audit", nil))
	if rr.Code != 404 {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestAuditRunningDirect(t *testing.T) {
	spec := &vm.Spec{Name: "dev1", Role: "user", User: "dev", CPUs: 1, MemMB: 128, ClusterID: 1, IP: "100.99.1.10", MAC: "10:65:30:01:00:0a"}
	s := newAuditTestServer(t, spec)
	if _, err := s.Clusters.Ensure(cluster.DefaultClusterID); err != nil {
		t.Fatal(err)
	}
	markAuditVMRunning(t, s, spec)
	guest := &fakeGuestRunner{t: t, fn: func(cmd string) (string, error) {
		switch {
		case strings.HasPrefix(cmd, "getent hosts "):
			return "198.18.0.7  pvm.probe\n", nil
		case strings.HasPrefix(cmd, "sysctl -n "):
			return "1\n", nil
		default:
			return "", fmt.Errorf("unexpected command %q", cmd)
		}
	}}

	rr := httptest.NewRecorder()
	s.auditVM(rr, auditRequest(), guest.run)
	if rr.Code != 200 {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	_, checks := decodeAuditChecks(t, rr.Body.Bytes())
	if c := requireAuditStatus(t, checks, "vm", "ok"); !strings.HasPrefix(c.Detail, "running (pid ") {
		t.Fatalf("vm detail = %q", c.Detail)
	}
	if c := requireAuditStatus(t, checks, "route", "ok"); c.Detail != "via direct" {
		t.Fatalf("route detail = %q", c.Detail)
	}
	requireAuditStatus(t, checks, "udp", "skipped")
	requireAuditStatus(t, checks, "icmp", "skipped")
	if c := requireAuditStatus(t, checks, "dns", "ok"); !strings.Contains(c.Detail, "198.18.0.7") {
		t.Fatalf("dns detail = %q", c.Detail)
	}
	requireAuditStatus(t, checks, "ipv6", "ok")
	requireAuditStatus(t, checks, "timezone", "skipped")
	requireAuditStatus(t, checks, "locale", "skipped")
	// Direct route: no probe traffic may be attempted.
	for _, cmd := range guest.cmds {
		if strings.HasPrefix(cmd, "ping ") || strings.HasPrefix(cmd, "python3 ") {
			t.Fatalf("direct vm must not run probe commands, got %q", cmd)
		}
	}
}

func TestAuditRunningExit(t *testing.T) {
	spec := &vm.Spec{Name: "dev1", Role: "user", User: "dev", CPUs: 1, MemMB: 128, ClusterID: 1, IP: "100.99.1.10", MAC: "10:65:30:01:00:0a",
		TZ: "America/Los_Angeles", Locale: "en_US.UTF-8"}
	s := newAuditTestServer(t, spec)
	if _, err := s.Clusters.Ensure(cluster.DefaultClusterID); err != nil {
		t.Fatal(err)
	}
	if err := s.Clusters.SetVMExit(cluster.DefaultClusterID, "dev1", "my-exit"); err != nil {
		t.Fatal(err)
	}
	if err := exit.SaveEntry(filepath.Join(s.P.Home, "exits"), &exit.Entry{
		Name: "my-exit", Server: "vps.example.com:18443", Transport: "udp",
	}); err != nil {
		t.Fatal(err)
	}
	markAuditVMRunning(t, s, spec)
	guest := &fakeGuestRunner{t: t, fn: func(cmd string) (string, error) {
		switch {
		case strings.HasPrefix(cmd, "ping -c 1 -W 3 pvm.probe"):
			return "64 bytes from 100.99.1.1: icmp_seq=1 ttl=64 time=0.5 ms\n", nil
		case strings.HasPrefix(cmd, "python3 -c "):
			return "", nil
		case strings.HasPrefix(cmd, "getent hosts "):
			return "203.0.113.5  pvm.probe\n", nil
		case strings.HasPrefix(cmd, "sysctl -n "):
			return "1\n", nil
		case strings.HasPrefix(cmd, "cat /etc/timezone"):
			return "America/Los_Angeles\n", nil
		case strings.HasPrefix(cmd, "grep -E "):
			return "en_US.UTF-8\n", nil
		default:
			return "", fmt.Errorf("unexpected command %q", cmd)
		}
	}}

	rr := httptest.NewRecorder()
	s.auditVM(rr, auditRequest(), guest.run)
	if rr.Code != 200 {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	_, checks := decodeAuditChecks(t, rr.Body.Bytes())
	for _, name := range []string{"vm", "route", "udp", "icmp", "dns", "ipv6", "timezone", "locale"} {
		requireAuditStatus(t, checks, name, "ok")
	}
	if c := checks["route"]; c.Detail != "via my-exit (per-vm)" {
		t.Fatalf("route detail = %q", c.Detail)
	}
	// Agent route: pvm.probe must resolve to the exit's real in-tunnel
	// address, not the reserved fake-ip.
	if c := checks["dns"]; !strings.Contains(c.Detail, "203.0.113.5") || !strings.Contains(c.Detail, "real-ip") {
		t.Fatalf("dns detail = %q, want the real exit address", c.Detail)
	}
	// The UDP probe must speak the exit-agent's magic+nonce protocol to
	// pvm.probe on exit.UDPProbePort.
	var udpCmd string
	for _, cmd := range guest.cmds {
		if strings.HasPrefix(cmd, "python3 ") {
			udpCmd = cmd
		}
	}
	if udpCmd == "" {
		t.Fatal("udp probe command not issued")
	}
	if !strings.Contains(udpCmd, `b"PVMP"+bytes.fromhex("`) {
		t.Fatalf("udp probe missing magic+nonce challenge: %q", udpCmd)
	}
	if !regexp.MustCompile(`bytes\.fromhex\("[0-9a-f]{32}"\)`).MatchString(udpCmd) {
		t.Fatalf("udp probe nonce must be 16 bytes hex: %q", udpCmd)
	}
	if !strings.Contains(udpCmd, `("pvm.probe",`+strconv.Itoa(exit.UDPProbePort)+`)`) {
		t.Fatalf("udp probe must target %s on exit.UDPProbePort: %q", "pvm.probe", udpCmd)
	}
	if !strings.Contains(udpCmd, "sys.exit(0 if d==p else 1)") {
		t.Fatalf("udp probe must validate the exact echo: %q", udpCmd)
	}
	// Guest inputs are whitelist constants: every issued command embeds
	// only the probe domain and fixed paths, never spec-derived strings.
	for _, cmd := range guest.cmds {
		if strings.Contains(cmd, spec.TZ) || strings.Contains(cmd, spec.Locale) {
			t.Fatalf("guest command embeds spec region data: %q", cmd)
		}
	}
}

func TestAuditClusterDefaultExit(t *testing.T) {
	spec := &vm.Spec{Name: "dev1", Role: "user", User: "dev", CPUs: 1, MemMB: 128, ClusterID: 1, IP: "100.99.1.10", MAC: "10:65:30:01:00:0a"}
	s := newAuditTestServer(t, spec)
	if _, err := s.Clusters.Ensure(cluster.DefaultClusterID); err != nil {
		t.Fatal(err)
	}
	if err := s.Clusters.SetDefaultExit(cluster.DefaultClusterID, "my-exit"); err != nil {
		t.Fatal(err)
	}
	if err := exit.SaveEntry(filepath.Join(s.P.Home, "exits"), &exit.Entry{
		Name: "my-exit", Server: "vps.example.com:18443", Transport: "udp",
	}); err != nil {
		t.Fatal(err)
	}
	markAuditVMRunning(t, s, spec)
	guest := &fakeGuestRunner{t: t, fn: func(cmd string) (string, error) {
		switch {
		case strings.HasPrefix(cmd, "ping "):
			return "", nil
		case strings.HasPrefix(cmd, "python3 -c "):
			return "", nil
		case strings.HasPrefix(cmd, "getent hosts "):
			return "203.0.113.9  pvm.probe\n", nil
		case strings.HasPrefix(cmd, "sysctl -n "):
			return "1\n", nil
		default:
			return "", fmt.Errorf("unexpected command %q", cmd)
		}
	}}

	rr := httptest.NewRecorder()
	s.auditVM(rr, auditRequest(), guest.run)
	if rr.Code != 200 {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	_, checks := decodeAuditChecks(t, rr.Body.Bytes())
	if c := requireAuditStatus(t, checks, "route", "ok"); c.Detail != "via my-exit (cluster default)" {
		t.Fatalf("route detail = %q", c.Detail)
	}
	requireAuditStatus(t, checks, "udp", "ok")
	requireAuditStatus(t, checks, "icmp", "ok")
	// cluster default exit is still an agent route: real-ip expected
	if c := requireAuditStatus(t, checks, "dns", "ok"); !strings.Contains(c.Detail, "real-ip") {
		t.Fatalf("dns detail = %q", c.Detail)
	}
}

func TestAuditWarpSkipsProbes(t *testing.T) {
	spec := &vm.Spec{Name: "dev1", Role: "user", User: "dev", CPUs: 1, MemMB: 128, ClusterID: 1, IP: "100.99.1.10", MAC: "10:65:30:01:00:0a"}
	s := newAuditTestServer(t, spec)
	if _, err := s.Clusters.Ensure(cluster.DefaultClusterID); err != nil {
		t.Fatal(err)
	}
	if err := s.Clusters.SetVMExit(cluster.DefaultClusterID, "dev1", "my-warp"); err != nil {
		t.Fatal(err)
	}
	if err := exit.SaveEntry(filepath.Join(s.P.Home, "exits"), &exit.Entry{
		Name: "my-warp", Provider: exit.ProviderWARP, Server: "api.cloudflareclient.com", Transport: "udp",
		WARP: &exit.WARPAccount{DeviceID: "device-1", Token: "cloudflare-api-token"},
	}); err != nil {
		t.Fatal(err)
	}
	markAuditVMRunning(t, s, spec)
	guest := &fakeGuestRunner{t: t, fn: func(cmd string) (string, error) {
		switch {
		case strings.HasPrefix(cmd, "getent hosts "):
			return "198.18.0.7  pvm.probe\n", nil
		case strings.HasPrefix(cmd, "sysctl -n "):
			return "1\n", nil
		default:
			return "", fmt.Errorf("unexpected command %q", cmd)
		}
	}}

	rr := httptest.NewRecorder()
	s.auditVM(rr, auditRequest(), guest.run)
	if rr.Code != 200 {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	_, checks := decodeAuditChecks(t, rr.Body.Bytes())
	requireAuditStatus(t, checks, "route", "ok")
	for _, name := range []string{"udp", "icmp"} {
		c := requireAuditStatus(t, checks, name, "skipped")
		if !strings.Contains(c.Detail, "warp") {
			t.Fatalf("%s detail = %q", name, c.Detail)
		}
	}
	requireAuditStatus(t, checks, "dns", "ok")
	for _, cmd := range guest.cmds {
		if strings.HasPrefix(cmd, "ping ") || strings.HasPrefix(cmd, "python3 ") {
			t.Fatalf("warp route must not run probe commands, got %q", cmd)
		}
	}
}

func TestAuditProbeFailures(t *testing.T) {
	spec := &vm.Spec{Name: "dev1", Role: "user", User: "dev", CPUs: 1, MemMB: 128, ClusterID: 1, IP: "100.99.1.10", MAC: "10:65:30:01:00:0a",
		TZ: "America/Los_Angeles", Locale: "en_US.UTF-8"}
	s := newAuditTestServer(t, spec)
	if _, err := s.Clusters.Ensure(cluster.DefaultClusterID); err != nil {
		t.Fatal(err)
	}
	if err := s.Clusters.SetVMExit(cluster.DefaultClusterID, "dev1", "my-exit"); err != nil {
		t.Fatal(err)
	}
	if err := exit.SaveEntry(filepath.Join(s.P.Home, "exits"), &exit.Entry{
		Name: "my-exit", Server: "vps.example.com:18443", Transport: "udp",
	}); err != nil {
		t.Fatal(err)
	}
	markAuditVMRunning(t, s, spec)
	guest := &fakeGuestRunner{t: t, fn: func(cmd string) (string, error) {
		switch {
		case strings.HasPrefix(cmd, "ping "):
			return "", errors.New("exit status 1")
		case strings.HasPrefix(cmd, "python3 -c "):
			return "", errors.New("exit status 1")
		case strings.HasPrefix(cmd, "getent hosts "):
			return "198.18.0.7  pvm.probe\n", nil
		case strings.HasPrefix(cmd, "sysctl -n "):
			return "0\n", nil
		case strings.HasPrefix(cmd, "cat /etc/timezone"):
			return "UTC\n", nil
		case strings.HasPrefix(cmd, "grep -E "):
			return "C.UTF-8\n", nil
		default:
			return "", fmt.Errorf("unexpected command %q", cmd)
		}
	}}

	rr := httptest.NewRecorder()
	s.auditVM(rr, auditRequest(), guest.run)
	if rr.Code != 200 {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	_, checks := decodeAuditChecks(t, rr.Body.Bytes())
	for _, name := range []string{"udp", "icmp", "dns"} {
		requireAuditStatus(t, checks, name, "fail")
	}
	for _, name := range []string{"ipv6", "timezone", "locale"} {
		requireAuditStatus(t, checks, name, "warn")
	}
	requireAuditStatus(t, checks, "vm", "ok")
	requireAuditStatus(t, checks, "route", "ok")
}

func TestAuditUnregisteredExit(t *testing.T) {
	spec := &vm.Spec{Name: "dev1", Role: "user", User: "dev", CPUs: 1, MemMB: 128, ClusterID: 1, IP: "100.99.1.10", MAC: "10:65:30:01:00:0a"}
	s := newAuditTestServer(t, spec)
	if _, err := s.Clusters.Ensure(cluster.DefaultClusterID); err != nil {
		t.Fatal(err)
	}
	if err := s.Clusters.SetVMExit(cluster.DefaultClusterID, "dev1", "gone-exit"); err != nil {
		t.Fatal(err)
	}
	markAuditVMRunning(t, s, spec)
	guest := &fakeGuestRunner{t: t, fn: func(cmd string) (string, error) {
		switch {
		case strings.HasPrefix(cmd, "getent hosts "):
			return "198.18.0.7  pvm.probe\n", nil
		case strings.HasPrefix(cmd, "sysctl -n "):
			return "1\n", nil
		default:
			return "", fmt.Errorf("unexpected command %q", cmd)
		}
	}}

	rr := httptest.NewRecorder()
	s.auditVM(rr, auditRequest(), guest.run)
	if rr.Code != 200 {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	_, checks := decodeAuditChecks(t, rr.Body.Bytes())
	requireAuditStatus(t, checks, "route", "warn")
	requireAuditStatus(t, checks, "udp", "skipped")
	requireAuditStatus(t, checks, "icmp", "skipped")
	requireAuditStatus(t, checks, "dns", "ok")
}

func TestAuditMissingToolsSkipped(t *testing.T) {
	spec := &vm.Spec{Name: "dev1", Role: "user", User: "dev", CPUs: 1, MemMB: 128, ClusterID: 1, IP: "100.99.1.10", MAC: "10:65:30:01:00:0a"}
	s := newAuditTestServer(t, spec)
	if _, err := s.Clusters.Ensure(cluster.DefaultClusterID); err != nil {
		t.Fatal(err)
	}
	if err := s.Clusters.SetVMExit(cluster.DefaultClusterID, "dev1", "my-exit"); err != nil {
		t.Fatal(err)
	}
	if err := exit.SaveEntry(filepath.Join(s.P.Home, "exits"), &exit.Entry{
		Name: "my-exit", Server: "vps.example.com:18443", Transport: "udp",
	}); err != nil {
		t.Fatal(err)
	}
	markAuditVMRunning(t, s, spec)
	guest := &fakeGuestRunner{t: t, fn: func(cmd string) (string, error) {
		switch {
		case strings.HasPrefix(cmd, "ping "):
			return "", errors.New("exit status 127")
		case strings.HasPrefix(cmd, "python3 -c "):
			return "", errors.New("bash: line 1: python3: command not found")
		case strings.HasPrefix(cmd, "getent hosts "):
			return "", errors.New("exit status 127")
		case strings.HasPrefix(cmd, "sysctl -n "):
			return "1\n", nil
		default:
			return "", fmt.Errorf("unexpected command %q", cmd)
		}
	}}

	rr := httptest.NewRecorder()
	s.auditVM(rr, auditRequest(), guest.run)
	if rr.Code != 200 {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	_, checks := decodeAuditChecks(t, rr.Body.Bytes())
	// Missing tools are unsupported, not probe failures.
	requireAuditStatus(t, checks, "udp", "skipped")
	requireAuditStatus(t, checks, "icmp", "skipped")
	requireAuditStatus(t, checks, "dns", "skipped")
	requireAuditStatus(t, checks, "ipv6", "ok")
}

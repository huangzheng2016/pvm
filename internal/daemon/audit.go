package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/exit"
	"github.com/huangzheng2016/pvm/internal/vm"
)

// probeDomain is the reserved probe name: on pvm exit-agent routes the
// gateway resolves it to the exit's real in-tunnel server address, so a
// successful TCP/UDP/ICMP probe proves the whole tunnel path works over L3
// forwarding; direct and WARP routes keep a reserved fake-ip answer. It
// never resolves publicly.
const probeDomain = "pvm.probe"

// fakeIPCIDR is the gateway's fake-ip pool; direct/WARP DNS checks
// expect the probe name to resolve inside it.
var fakeIPCIDR = netip.MustParsePrefix("198.18.0.0/16")

// Check statuses. fail means the probe ran and the target is broken;
// warn means a config drift or degraded defense; skipped means the check
// does not apply or cannot run here. Only fail makes `pvm audit` exit
// non-zero.
const (
	auditOK      = "ok"
	auditWarn    = "warn"
	auditFail    = "fail"
	auditSkipped = "skipped"
)

type auditCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

func check(name, status, format string, args ...any) auditCheck {
	return auditCheck{Name: name, Status: status, Detail: fmt.Sprintf(format, args...)}
}

// guestRunner executes a command in the guest over ssh. Tests swap it for
// a fake so audit logic runs without a real VM.
type guestRunner func(spec *vm.Spec, cmd string) (string, error)

// handleVMAudit is strictly read-only: it never starts or stops the VM,
// never touches routes, and never blocks on readiness.
func (s *Server) handleVMAudit(w http.ResponseWriter, r *http.Request) {
	s.auditVM(w, r, s.sshRun)
}

func (s *Server) auditVM(w http.ResponseWriter, r *http.Request, run guestRunner) {
	spec, err := s.getSpec(r.PathValue("name"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	st, err := s.instance(spec).Running()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if st == nil {
		checks := []auditCheck{check("vm", auditWarn, "stopped")}
		for _, name := range []string{"route", "tcp", "udp", "icmp", "dns", "ipv6", "timezone", "locale"} {
			checks = append(checks, check(name, auditSkipped, "vm not running"))
		}
		writeJSON(w, 200, checks)
		return
	}

	checks := []auditCheck{check("vm", auditOK, "running (pid %d, uptime %s)", st.PID, time.Since(st.Started).Round(time.Second))}
	routeChk, entry := s.auditRoute(spec)
	checks = append(checks, routeChk)
	checks = append(checks, s.auditProbes(spec, routeChk, entry, run)...)
	checks = append(checks,
		auditIPv6(run, spec),
		auditTimezone(run, spec),
		auditLocale(run, spec))
	writeJSON(w, 200, checks)
}

// auditRoute reports the VM's current route and, when it points at a
// registered exit, the loaded entry for the probe checks.
func (s *Server) auditRoute(spec *vm.Spec) (auditCheck, *exit.Entry) {
	if spec.Role == "gateway" {
		return check("route", auditOK, "gateway vm"), nil
	}
	if spec.ClusterID == 0 {
		return check("route", auditOK, "via direct (standalone)"), nil
	}
	cl := s.Clusters.Get(spec.ClusterID)
	if cl == nil {
		return check("route", auditWarn, "cluster %d state missing", spec.ClusterID), nil
	}
	via, source := cl.VMExits[spec.Name], "per-vm"
	if via == "" {
		via, source = cl.DefaultExit, "cluster default"
	}
	if via == "" || via == "direct" {
		if source == "per-vm" {
			return check("route", auditOK, "via direct (per-vm)"), nil
		}
		return check("route", auditOK, "via direct"), nil
	}
	entry, err := exit.LoadEntry(filepath.Join(s.P.Home, "exits"), via)
	if err != nil {
		return check("route", auditWarn, "via %s (%s) is not registered", via, source), nil
	}
	return check("route", auditOK, "via %s (%s)", via, source), entry
}

// auditProbes runs the TCP/UDP/ICMP tunnel probes and the route-aware DNS
// check. Only a VM routed through its own pvm exit-agent gets probes: direct
// and WARP routes have no probe service, and third-party targets are never
// substituted.
func (s *Server) auditProbes(spec *vm.Spec, routeChk auditCheck, entry *exit.Entry, run guestRunner) []auditCheck {
	if spec.Role == "gateway" || spec.ClusterID == 0 {
		return []auditCheck{
			check("tcp", auditSkipped, "no cluster gateway"),
			check("udp", auditSkipped, "no cluster gateway"),
			check("icmp", auditSkipped, "no cluster gateway"),
			check("dns", auditSkipped, "no cluster gateway"),
		}
	}
	if entry == nil {
		reason := "direct route has no probe agent"
		if routeChk.Status == auditWarn {
			reason = "route target unavailable"
		}
		return []auditCheck{
			check("tcp", auditSkipped, "%s", reason),
			check("udp", auditSkipped, "%s", reason),
			check("icmp", auditSkipped, "%s", reason),
			auditDNS(run, spec, false),
		}
	}
	if entry.IsWARP() {
		return []auditCheck{
			check("tcp", auditSkipped, "warp exits have no probe agent"),
			check("udp", auditSkipped, "warp exits have no probe agent"),
			check("icmp", auditSkipped, "warp exits have no probe agent"),
			auditDNS(run, spec, false),
		}
	}
	return []auditCheck{auditTCP(run, spec), auditUDP(run, spec), auditICMP(run, spec), auditDNS(run, spec, true)}
}

// auditTCP opens a TCP connection to the exit-agent's in-tunnel HTTP
// service, requests /ping and requires an HTTP 200 status line, so connect
// success alone never counts: the exit must actually answer. pvm.probe
// resolves to the server's real tunnel address on agent routes, so this
// exercises the guest's L3 forwarding through the tunnel.
func auditTCP(run guestRunner, spec *vm.Spec) auditCheck {
	script := `python3 -c 'import socket,sys;` +
		`s=socket.create_connection(("` + probeDomain + `",` + strconv.Itoa(exit.SpeedPort) + `),5);` +
		`s.settimeout(5);` +
		`s.sendall(b"GET /ping HTTP/1.0\r\n\r\n");` +
		`l=s.makefile("rb").readline(128).strip().split();` +
		`sys.exit(0 if len(l)>1 and l[0].startswith(b"HTTP/") and l[1]==b"200" else 1)'`
	if _, err := run(spec, script); err != nil {
		if isCmdMissing(err) {
			return check("tcp", auditSkipped, "python3 unavailable in guest")
		}
		return check("tcp", auditFail, "no valid /ping response: %v", err)
	}
	return check("tcp", auditOK, "/ping via %s:%d", probeDomain, exit.SpeedPort)
}

// auditUDP challenges the exit-agent's probe service with a magic+nonce
// datagram (mirroring the exit package's udpProbeMagic protocol, whose
// listener only answers exact-size magic+nonce datagrams) and requires the
// byte-exact echo back. An exact echo proves the UDP path works end to
// end; no response is never ok.
func auditUDP(run guestRunner, spec *vm.Spec) auditCheck {
	nonce, err := probeNonce()
	if err != nil {
		return check("udp", auditFail, "nonce: %v", err)
	}
	script := `python3 -c 'import socket,sys;p=b"PVMP"+bytes.fromhex("` + nonce + `");` +
		`s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.settimeout(5);` +
		`s.sendto(p,("` + probeDomain + `",` + strconv.Itoa(exit.UDPProbePort) + `));` +
		`d,_=s.recvfrom(4096);sys.exit(0 if d==p else 1)'`
	if _, err := run(spec, script); err != nil {
		if isCmdMissing(err) {
			return check("udp", auditSkipped, "python3 unavailable in guest")
		}
		return check("udp", auditFail, "no valid echo: %v", err)
	}
	return check("udp", auditOK, "nonce echoed via %s:%d", probeDomain, exit.UDPProbePort)
}

func auditICMP(run guestRunner, spec *vm.Spec) auditCheck {
	if _, err := run(spec, "ping -c 1 -W 3 "+probeDomain); err != nil {
		if isCmdMissing(err) {
			return check("icmp", auditSkipped, "ping unavailable in guest")
		}
		return check("icmp", auditFail, "no echo reply: %v", err)
	}
	return check("icmp", auditOK, "echo reply via %s", probeDomain)
}

// auditDNS requires pvm.probe to resolve the way the route demands: a
// real (non-fake) IPv4 for pvm exit-agent routes — the name maps to the
// server's in-tunnel address so the probes exercise L3 forwarding — and
// the reserved fake-ip for direct/WARP routes, where a real answer would
// mean guest DNS is bypassing the gateway.
func auditDNS(run guestRunner, spec *vm.Spec, agentRoute bool) auditCheck {
	out, err := run(spec, "getent hosts "+probeDomain)
	if err != nil {
		if isCmdMissing(err) {
			return check("dns", auditSkipped, "getent unavailable in guest")
		}
		return check("dns", auditFail, "cannot resolve %s: %v", probeDomain, err)
	}
	ip := firstField(out)
	if ip == "" {
		return check("dns", auditFail, "empty getent response")
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil || !addr.Is4() {
		return check("dns", auditFail, "%s resolved to %s, want IPv4", probeDomain, ip)
	}
	if agentRoute {
		if fakeIPCIDR.Contains(addr) {
			return check("dns", auditFail, "%s resolved to fake-ip %s, want the exit's tunnel address", probeDomain, ip)
		}
		return check("dns", auditOK, "%s -> %s (real-ip)", probeDomain, ip)
	}
	if !fakeIPCIDR.Contains(addr) {
		return check("dns", auditFail, "%s resolved to %s, want fake-ip in %s", probeDomain, ip, fakeIPCIDR)
	}
	return check("dns", auditOK, "%s -> %s (fake-ip)", probeDomain, ip)
}

func auditIPv6(run guestRunner, spec *vm.Spec) auditCheck {
	out, err := run(spec, "sysctl -n net.ipv6.conf.all.disable_ipv6")
	if err != nil {
		if isCmdMissing(err) {
			return check("ipv6", auditSkipped, "sysctl unavailable in guest")
		}
		return check("ipv6", auditSkipped, "cannot determine: %v", err)
	}
	switch strings.TrimSpace(out) {
	case "1":
		return check("ipv6", auditOK, "disabled")
	case "0":
		// The gateway drops guest IPv6 at the bridge, so this degrades
		// connectivity rather than leaking; still worth surfacing.
		return check("ipv6", auditWarn, "enabled; guest ipv6 is dropped by the gateway")
	default:
		return check("ipv6", auditSkipped, "unexpected sysctl value %q", strings.TrimSpace(out))
	}
}

func auditTimezone(run guestRunner, spec *vm.Spec) auditCheck {
	if spec.TZ == "" {
		return check("timezone", auditSkipped, "no tz in spec")
	}
	out, err := run(spec, "cat /etc/timezone 2>/dev/null")
	cur := strings.TrimSpace(out)
	if err != nil || cur == "" {
		return check("timezone", auditSkipped, "cannot determine guest tz")
	}
	if cur != spec.TZ {
		return check("timezone", auditWarn, "guest %s != spec %s", cur, spec.TZ)
	}
	return check("timezone", auditOK, "%s", cur)
}

// auditLocale compares the guest's session LANG (the persistent value pvm
// writes via update-locale / pvm-locale.sh) with the spec.
func auditLocale(run guestRunner, spec *vm.Spec) auditCheck {
	if spec.Locale == "" {
		return check("locale", auditSkipped, "no locale in spec")
	}
	out, err := run(spec, `grep -E '^(export )?LANG=' /etc/default/locale /etc/profile.d/pvm-locale.sh 2>/dev/null | tail -1 | sed 's/^export //' | cut -d= -f2`)
	cur := strings.Trim(strings.TrimSpace(out), `"'`)
	if err != nil || cur == "" {
		return check("locale", auditSkipped, "cannot determine guest lang")
	}
	if cur != spec.Locale {
		return check("locale", auditWarn, "guest LANG=%s != spec %s", cur, spec.Locale)
	}
	return check("locale", auditOK, "LANG=%s", cur)
}

func probeNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func firstField(out string) string {
	for line := range strings.Lines(out) {
		if f := strings.Fields(line); len(f) > 0 {
			return f[0]
		}
	}
	return ""
}

// isCmdMissing reports whether the guest lacks the tool entirely
// (unsupported) as opposed to the probe itself failing.
func isCmdMissing(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "command not found") || strings.Contains(msg, "exit status 127")
}

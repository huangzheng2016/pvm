package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/agent"
	"github.com/huangzheng2016/pvm/internal/exit"
	"github.com/huangzheng2016/pvm/internal/fsutil"
	"github.com/huangzheng2016/pvm/internal/gateway"
	"github.com/huangzheng2016/pvm/internal/netcheck"
	"github.com/huangzheng2016/pvm/internal/vm"
)

const guestCheckScript = `import json, os, socket, subprocess, time, urllib.request

def tcp_probe(host, port):
    try:
        s = socket.create_connection((host, port), timeout=2)
        s.close()
        return "connected"
    except Exception as e:
        return type(e).__name__

v6 = subprocess.run(["ip", "-6", "addr", "show", "scope", "global"], capture_output=True, text=True).stdout
resolv = open("/etc/resolv.conf").read() if os.path.exists("/etc/resolv.conf") else ""
exit_ip = ""
try:
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    exit_ip = opener.open("https://api.ipify.org/", timeout=8).read(128).decode().strip()
except Exception as e:
    exit_ip = "error: " + type(e).__name__
print(json.dumps({
    "boot_id": open("/proc/sys/kernel/random/boot_id").read().strip(),
    "resolv": [l for l in resolv.splitlines() if l.startswith("nameserver")],
    "v6_global": v6.count("inet6 "),
    "meta": tcp_probe("169.254.169.254", 80),
    "dot": tcp_probe("1.1.1.1", 853),
    "private": tcp_probe("192.168.255.1", 80),
    "exit_ip": exit_ip,
    "tz": os.environ.get("TZ", ""),
    "lang": os.environ.get("LANG", ""),
    "lc_all": os.environ.get("LC_ALL", ""),
    "utc_offset": -time.timezone if not time.localtime().tm_isdst else -time.altzone,
}))
`

type guestCheckResult struct {
	BootID    string   `json:"boot_id"`
	Resolv    []string `json:"resolv"`
	V6Global  int      `json:"v6_global"`
	Meta      string   `json:"meta"`
	DoT       string   `json:"dot"`
	Private   string   `json:"private"`
	ExitIP    string   `json:"exit_ip"`
	TZ        string   `json:"tz"`
	Lang      string   `json:"lang"`
	LCAll     string   `json:"lc_all"`
	UTCOffset int      `json:"utc_offset"`
}

func (s *Server) runGuestCapture(ctx context.Context, name string, argv []string, stdin string, timeoutS int) (string, int, error) {
	ac, err := s.agentClientContext(ctx, name)
	if err != nil {
		return "", -1, err
	}
	var out bytes.Buffer
	exitCode := -1
	var cmdErr string
	_, err = ac.ExecStreamContext(ctx, argv, strings.NewReader(stdin), timeoutS, func(f *agent.Frame) {
		if f.Stream == "stdout" && f.DataB64 != "" {
			if b, derr := base64.StdEncoding.DecodeString(f.DataB64); derr == nil {
				out.Write(b)
			}
		}
		if f.Error != "" {
			cmdErr = f.Error
		}
		if f.Done {
			exitCode = f.ExitCode
		}
	})
	if err != nil {
		return out.String(), exitCode, err
	}
	if cmdErr != "" {
		return out.String(), exitCode, errors.New(cmdErr)
	}
	return out.String(), exitCode, nil
}

func (s *Server) checkPath(name string) string {
	return s.P.VMDir(name) + "/last-check.json"
}

func (s *Server) handleExitProbe(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cl := s.Clusters.Get(1)
	if cl == nil {
		writeErr(w, 409, fmt.Errorf("no cluster yet; create a VM first"))
		return
	}
	gw, err := s.getSpec(cl.Gateway)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	out, err := s.sshRunCtx(ctx, gw, s.gatewayBinCmd("__probe-exit", "--exit", name, "--geo"))
	if err != nil {
		writeErr(w, 502, fmt.Errorf("probe exit %s: %w", name, err))
		return
	}
	var res gateway.ExitProbeResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		writeErr(w, 502, err)
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) handleVMCheck(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if r.Method == http.MethodGet {
		raw, err := os.ReadFile(s.checkPath(name))
		if err != nil {
			writeErr(w, 404, fmt.Errorf("no check report yet; run `pvm check %s`", name))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
		return
	}
	spec, err := s.getSpec(name)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	if st, err := s.instance(spec).Running(); err != nil || st == nil {
		writeErr(w, 409, fmt.Errorf("vm %s is not running", name))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	rep, err := s.runVMCheck(ctx, spec)
	if err != nil {
		writeErr(w, 502, err)
		return
	}
	data, _ := json.MarshalIndent(rep, "", "  ")
	if err := fsutil.AtomicWrite(s.checkPath(name), data, 0o600); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, rep)
}

func (s *Server) gatewayBinCmd(sub string, args ...string) string {
	parts := []string{"sudo", "/usr/local/bin/pvm-gateway", sub}
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

func readGatewayCounters(ctx context.Context, s *Server, gw *vm.Spec) (map[string]uint64, error) {
	out, err := s.sshRunCtx(ctx, gw, s.gatewayBinCmd("__counters"))
	if err != nil {
		return nil, err
	}
	var m map[string]uint64
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		return nil, err
	}
	return m, nil
}

func (s *Server) runVMCheck(ctx context.Context, spec *vm.Spec) (*netcheck.Report, error) {
	cl := s.Clusters.Get(spec.ClusterID)
	if cl == nil {
		return nil, fmt.Errorf("vm %s has no cluster", spec.Name)
	}
	gw, err := s.getSpec(cl.Gateway)
	if err != nil {
		return nil, err
	}
	cfgBytes := gatewayConfigFromCluster(s, cl)
	routeSum := sha256.Sum256(append(cfgBytes, []byte(":"+spec.Name)...))
	routeHash := hex.EncodeToString(routeSum[:8])

	effectiveExit := cl.VMExits[spec.Name]
	if effectiveExit == "" {
		effectiveExit = cl.DefaultExit
	}
	if effectiveExit == "" {
		effectiveExit = "direct"
	}

	before, errBefore := readGatewayCounters(ctx, s, gw)
	out, code, err := s.runGuestCapture(ctx, spec.Name, []string{"python3", "-c", guestCheckScript}, "", 25)
	if err != nil {
		return nil, fmt.Errorf("guest check probe: %w", err)
	}
	if code != 0 {
		return nil, fmt.Errorf("guest check probe exited %d: %s", code, strings.TrimSpace(out))
	}
	var g guestCheckResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &g); err != nil {
		return nil, fmt.Errorf("parse guest check probe: %w (output %q)", err, out)
	}
	after, errAfter := readGatewayCounters(ctx, s, gw)

	var gwProbe gateway.ExitProbeResult
	var probeErr error
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	pOut, err := s.sshRunCtx(probeCtx, gw, s.gatewayBinCmd("__probe-exit", "--exit", effectiveExit, "--geo"))
	cancel()
	if err != nil {
		probeErr = err
	} else if err := json.Unmarshal([]byte(pOut), &gwProbe); err != nil {
		probeErr = err
	}

	rep := netcheck.NewReport(spec.Name, g.BootID, routeHash)
	now := time.Now().UTC()
	add := func(id, title, state, evidence, remedy string) {
		rep.Checks = append(rep.Checks, netcheck.CheckItem{
			ID: id, Title: title, State: state, Critical: true, Evidence: evidence, Remedy: remedy, CheckedAt: now,
		})
	}

	// 1. DNS resolver confinement
	dnsOK := false
	for _, line := range g.Resolv {
		if strings.TrimSpace(line) == "nameserver "+cl.GwIP || strings.TrimSpace(line) == "nameserver 127.0.0.53" {
			dnsOK = true
		}
	}
	if dnsOK {
		add("dns_resolver", "Guest DNS resolver points to the gateway", netcheck.StatePassed, strings.Join(g.Resolv, "; "), "")
	} else if len(g.Resolv) > 0 {
		add("dns_resolver", "Guest DNS resolver points to the gateway", netcheck.StateFailed, "unexpected resolv.conf: "+strings.Join(g.Resolv, "; "), "set DNS to the cluster gateway "+cl.GwIP)
	} else {
		add("dns_resolver", "Guest DNS resolver points to the gateway", netcheck.StateUnknown, "no nameserver lines observed", "check /etc/resolv.conf in the guest")
	}

	// 2. Negative probes cross-checked with gateway nft counters
	counterCheck := func(id, title, counterName, guestState, remedy string) {
		if errBefore != nil || errAfter != nil {
			add(id, title, netcheck.StateUnknown, fmt.Sprintf("guest probe=%s; counter read error: %v %v", guestState, errBefore, errAfter), "check gateway nft table and pvm-gateway __counters")
			return
		}
		delta := after[counterName] - before[counterName]
		ev := fmt.Sprintf("guest probe=%s; gateway %s +%d packets", guestState, counterName, delta)
		switch {
		case guestState == "connected":
			add(id, title, netcheck.StateFailed, ev, remedy)
		case delta > 0:
			add(id, title, netcheck.StatePassed, ev, "")
		default:
			add(id, title, netcheck.StateUnknown, ev, "gateway counter did not record the probe; check gateway nft rules")
		}
	}
	counterCheck("metadata_boundary", "Cloud metadata and link-local are blocked", "pvm_drop_meta", g.Meta, "block 169.254.0.0/16 on the gateway")
	counterCheck("dot_boundary", "Direct DNS-over-TLS (853) is blocked", "pvm_drop_dot_tcp", g.DoT, "use gateway DNS instead of hardcoded DoT")
	counterCheck("private_boundary", "Host LAN and private ranges are blocked on direct egress", "pvm_drop_private", g.Private, "only allow private access via explicit `pvm route add direct --cidr`")

	// 3. IPv6 confinement
	if g.V6Global == 0 {
		add("ipv6_policy", "Guest has no global IPv6 addresses", netcheck.StatePassed, "global IPv6 address count = 0", "")
	} else {
		add("ipv6_policy", "Guest has no global IPv6 addresses", netcheck.StateFailed, fmt.Sprintf("global IPv6 address count = %d", g.V6Global), "set net.ipv6.conf.all.disable_ipv6=1")
	}

	// 4. Two-path public exit sampling (guest terminal + gateway SO_MARK)
	guestIP := net.ParseIP(g.ExitIP)
	guestOK := guestIP != nil && guestIP.To4() != nil && !exit.IsBlockedEgressIP(guestIP)
	switch {
	case probeErr != nil:
		add("exit_consistency", "Guest and gateway exit IP sampling agree", netcheck.StateUnknown, "gateway probe error: "+probeErr.Error(), "check gateway connectivity")
	case !guestOK:
		add("exit_consistency", "Guest and gateway exit IP sampling agree", netcheck.StateUnknown, "guest sample="+g.ExitIP+" gateway="+gwProbe.IP, "check guest outbound connectivity")
	case gwProbe.Geo != nil && strings.EqualFold(gwProbe.Geo.CountryCode, "CN"):
		add("exit_consistency", "Guest and gateway exit IP sampling agree", netcheck.StateFailed, "mainland China exit detected ("+gwProbe.IP+")", "select a non-mainland exit")
	case effectiveExit != "direct" && guestIP.String() != gwProbe.IP:
		add("exit_consistency", "Guest and gateway exit IP sampling agree", netcheck.StateFailed, fmt.Sprintf("guest=%s gateway=%s via %s", g.ExitIP, gwProbe.IP, effectiveExit), "guest traffic bypassed the selected exit")
	default:
		ev := fmt.Sprintf("guest=%s gateway=%s via=%s", g.ExitIP, gwProbe.IP, effectiveExit)
		if gwProbe.Geo != nil {
			ev += fmt.Sprintf(" country=%s tz=%s (%s)", gwProbe.Geo.CountryCode, gwProbe.Geo.Timezone, gwProbe.Geo.Source)
		}
		add("exit_consistency", "Guest and gateway exit IP sampling agree", netcheck.StatePassed, ev, "")
	}

	// 5. Timezone and locale evidence
	ev := fmt.Sprintf("LANG=%s LC_ALL=%s TZ=%s utc_offset=%ds", g.Lang, g.LCAll, g.TZ, g.UTCOffset)
	if g.Lang != "" {
		add("guest_settings", "Guest locale and timezone observed", netcheck.StatePassed, ev, "")
	} else {
		add("guest_settings", "Guest locale and timezone observed", netcheck.StateUnknown, ev, "check guest locale configuration")
	}
	return rep, nil
}

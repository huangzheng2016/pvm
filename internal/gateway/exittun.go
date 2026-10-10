package gateway

import (
	"context"
	"fmt"
	"log"
	"net"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/huangzheng2016/pvm/internal/exit"
	"github.com/huangzheng2016/pvm/internal/tunnel"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
)

// ExitDef is everything the gateway needs to build a tunnel to one
// exit-node (the daemon pushes these for exits referenced by rules or
// per-VM routes).
type ExitDef struct {
	Server      string `json:"server"`      // control plane host:port
	Token       string `json:"token"`       // control plane bearer token
	Transport   string `json:"transport"`   // udp | tcp
	Fingerprint string `json:"fingerprint"` // pinned control-plane cert fingerprint (TOFU)
}

const (
	rulePrefBypass  = 50 // direct CIDRs / direct-domain IPs -> main table
	rulePrefVMExit  = 101
	fwMarkBase      = 0x99 // exit idx i uses mark fwMarkBase+i, table 100+i
	ruleTableBase   = 100
	watchdogPeriod  = 20 * time.Second
	watchdogMaxDown = 3 // consecutive failed probes before re-enroll
)

// exitIndex returns the deterministic tunnel index for an exit name:
// the position in the sorted exit-name list. The nft renderer uses the
// same ordering to name interfaces.
func exitIndex(exitNames []string, name string) int {
	sorted := append([]string(nil), exitNames...)
	sort.Strings(sorted)
	for i, n := range sorted {
		if n == name {
			return i
		}
	}
	return -1
}

// tunnelIfName maps a tunnel index to its interface name.
func tunnelIfName(idx int) string { return fmt.Sprintf("pvmwg%d", idx) }

// exitTunnel is one live WireGuard client device to an exit-node.
type exitTunnel struct {
	name   string
	def    ExitDef
	idx    int
	tunIP  string
	dev    *device.Device
	td     tun.Device
	stop   chan struct{}
	done   chan struct{}
	server string // data-plane endpoint host (for the anti-loop host rule)
}

// lastHandshake reports the newest peer handshake time (unix seconds).
func (t *exitTunnel) lastHandshake() int64 {
	out, err := t.dev.IpcGet()
	if err != nil {
		return 0
	}
	var latest int64
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "last_handshake_time_sec=") {
			v, _ := strconv.ParseInt(strings.TrimPrefix(line, "last_handshake_time_sec="), 10, 64)
			if v > latest {
				latest = v
			}
		}
	}
	return latest
}

// exitManager owns all exit tunnels and the policy-routing state that
// sends marked/from-VM traffic into them.
type exitManager struct {
	mu     sync.Mutex
	tuns   map[string]*exitTunnel
	// desired ip-rule state (reconciled on Sync)
	vmRules   map[string]int  // vmIP -> exit idx
	cidrRules map[string]int  // proxied cidr -> exit idx
	bypass    map[string]bool // direct cidrs/ips -> lookup main
	// onBypass fires when a new bypass ip/cidr is learned at runtime
	// (direct-domain DNS answers), so the nft bypass set stays in sync
	// between full table rebuilds.
	onBypass func(ip string)
}

func newExitManager() *exitManager {
	return &exitManager{
		tuns:      map[string]*exitTunnel{},
		vmRules:   map[string]int{},
		cidrRules: map[string]int{},
		bypass:    map[string]bool{},
	}
}

// Sync converges tunnels and ip rules to the config. exits contains
// only the defs that are referenced; vmExits is vmName -> exit name
// ("direct"/"" handled by caller); vmIPs maps vmName -> cluster IP;
// proxiedCIDRs and directCIDRs come from the routing rules.
func (m *exitManager) Sync(exits map[string]ExitDef, vmExits map[string]string, vmIPs map[string]string, proxiedCIDRs, directCIDRs []string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	names := make([]string, 0, len(exits))
	for n := range exits {
		names = append(names, n)
	}

	// tear down tunnels no longer referenced
	for name, t := range m.tuns {
		if _, ok := exits[name]; !ok {
			m.teardownLocked(t)
		}
	}
	// ensure tunnels
	for name, def := range exits {
		if idx := exitIndex(names, name); idx >= 0 {
			m.ensureLocked(name, def, idx)
		}
	}

	// desired rule state
	wantVM := map[string]int{}
	for vm, exitName := range vmExits {
		idx := exitIndex(names, exitName)
		ip := vmIPs[vm]
		if idx >= 0 && ip != "" {
			wantVM[ip] = idx
		}
	}
	wantCIDR := map[string]int{}
	for _, c := range proxiedCIDRs {
		// proxied cidrs carry their exit in the rule; caller passes "cidr@exit"
		cidr, exitName, ok := strings.Cut(c, "@")
		if !ok {
			continue
		}
		if idx := exitIndex(names, exitName); idx >= 0 {
			wantCIDR[cidr] = idx
		}
	}
	wantBypass := map[string]bool{}
	for _, c := range directCIDRs {
		wantBypass[c] = true
	}
	for _, t := range m.tuns {
		if t.server != "" {
			wantBypass[t.server+"/32"] = true
		}
	}

	m.reconcileRulesLocked(wantVM, wantCIDR, wantBypass)
}

// AddBypassIP records a freshly resolved direct-domain IP so its
// traffic skips the tunnel even for proxied VMs.
func (m *exitManager) AddBypassIP(ip string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.bypass[ip+"/32"] {
		return
	}
	if err := ipRule("add", "to", ip+"/32", "lookup", "main", "pref", strconv.Itoa(rulePrefBypass)); err == nil {
		m.bypass[ip+"/32"] = true
		if m.onBypass != nil {
			m.onBypass(ip + "/32")
		}
	}
}

// BypassList snapshots the current bypass set (direct CIDRs, endpoint
// /32s and learned direct-domain IPs) for the nft full-table render.
func (m *exitManager) BypassList() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.bypass))
	for cidr := range m.bypass {
		out = append(out, cidr)
	}
	sort.Strings(out)
	return out
}

func (m *exitManager) reconcileRulesLocked(wantVM map[string]int, wantCIDR map[string]int, wantBypass map[string]bool) {
	for ip, idx := range m.vmRules {
		if _, ok := wantVM[ip]; !ok {
			ipRule("del", "from", ip+"/32", "lookup", strconv.Itoa(ruleTableBase+idx))
			delete(m.vmRules, ip)
		}
	}
	for ip, idx := range wantVM {
		if cur, ok := m.vmRules[ip]; !ok || cur != idx {
			ipRule("del", "from", ip+"/32", "lookup", strconv.Itoa(ruleTableBase+cur))
			if err := ipRule("add", "from", ip+"/32", "lookup", strconv.Itoa(ruleTableBase+idx), "pref", strconv.Itoa(rulePrefVMExit)); err == nil {
				m.vmRules[ip] = idx
			}
		}
	}
	for cidr, idx := range m.cidrRules {
		if _, ok := wantCIDR[cidr]; !ok {
			ipRule("del", "to", cidr, "lookup", strconv.Itoa(ruleTableBase+idx))
			delete(m.cidrRules, cidr)
		}
	}
	for cidr, idx := range wantCIDR {
		if cur, ok := m.cidrRules[cidr]; !ok || cur != idx {
			ipRule("del", "to", cidr, "lookup", strconv.Itoa(ruleTableBase+cur))
			if err := ipRule("add", "to", cidr, "lookup", strconv.Itoa(ruleTableBase+idx), "pref", "60"); err == nil {
				m.cidrRules[cidr] = idx
			}
		}
	}
	for cidr := range m.bypass {
		if !wantBypass[cidr] {
			ipRule("del", "to", cidr, "lookup", "main")
			delete(m.bypass, cidr)
		}
	}
	for cidr := range wantBypass {
		if !m.bypass[cidr] {
			if err := ipRule("add", "to", cidr, "lookup", "main", "pref", strconv.Itoa(rulePrefBypass)); err == nil {
				m.bypass[cidr] = true
			}
		}
	}
}

func (m *exitManager) ensureLocked(name string, def ExitDef, idx int) {
	if t, ok := m.tuns[name]; ok {
		if t.def == def {
			return // already up with the same parameters
		}
		m.teardownLocked(t)
	}
	t := &exitTunnel{name: name, def: def, idx: idx, stop: make(chan struct{}), done: make(chan struct{})}
	m.tuns[name] = t
	go m.runTunnel(t)
}

func (m *exitManager) teardownLocked(t *exitTunnel) {
	close(t.stop)
	<-t.done
	delete(m.tuns, t.name)
}

// runTunnel enrolls and maintains the tunnel until torn down,
// re-enrolling after prolonged handshake failure.
func (m *exitManager) runTunnel(t *exitTunnel) {
	defer close(t.done)
	backoff := 2 * time.Second
	for {
		select {
		case <-t.stop:
			return
		default:
		}
		if err := m.upOnce(t); err != nil {
			log.Printf("exit %s: tunnel up failed: %v (retry in %v)", t.name, err, backoff)
			select {
			case <-t.stop:
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = 2 * time.Second
		if down := m.watch(t); down {
			log.Printf("exit %s: watchdog detected dead tunnel, re-enrolling", t.name)
		}
		m.cleanupDevice(t)
	}
}

// upOnce enrolls a fresh peer and brings the device and routing up.
func (m *exitManager) upOnce(t *exitTunnel) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	priv, err := tunnel.GenerateKey()
	if err != nil {
		return err
	}
	pub, err := tunnel.PublicKey(priv)
	if err != nil {
		return err
	}
	cl := exit.NewClient(t.def.Server, t.def.Token, t.def.Fingerprint)
	hostname := "pvm-gw"
	resp, err := cl.Enroll(ctx, pub, hostname, t.def.Transport)
	if err != nil {
		return fmt.Errorf("enroll: %w", err)
	}
	mtu := resp.MTU
	if mtu == 0 {
		mtu = exit.MTUFor(t.def.Transport)
	}
	ifName := tunnelIfName(t.idx)
	td, err := tun.CreateTUN(ifName, mtu)
	if err != nil {
		return fmt.Errorf("create tun: %w", err)
	}
	dev, err := tunnel.NewDevice(td, tunnel.BindFor(t.def.Transport), false)
	if err != nil {
		td.Close()
		return fmt.Errorf("wg device: %w", err)
	}
	privHex, err := tunnel.Hex(priv)
	if err != nil {
		dev.Close()
		return err
	}
	pubHex, err := tunnel.Hex(resp.ServerPubKey)
	if err != nil {
		dev.Close()
		return err
	}
	conf := fmt.Sprintf(`private_key=%s
public_key=%s
endpoint=%s
allowed_ip=0.0.0.0/0
persistent_keepalive_interval=25
`, privHex, pubHex, resp.Endpoint)
	if err := dev.IpcSet(conf); err != nil {
		dev.Close()
		return fmt.Errorf("configure device: %w", err)
	}
	if err := exit.ConfigureTUN(ifName, resp.TunnelIP+"/32"); err != nil {
		dev.Close()
		return err
	}
	t.dev, t.td, t.tunIP = dev, td, resp.TunnelIP
	if host, _, err := net.SplitHostPort(resp.Endpoint); err == nil {
		t.server = host
	}

	// policy routing: default via this tunnel in its own table; marked
	// flows (fakeproxy proxy dials) select the table by fwmark.
	table := strconv.Itoa(ruleTableBase + t.idx)
	if err := ipRoute("replace", "default", "dev", ifName, "table", table); err != nil {
		dev.Close()
		return err
	}
	mark := strconv.Itoa(fwMarkBase + t.idx)
	ipRule("del", "fwmark", mark, "lookup", table) // dedupe, ignore error
	if err := ipRule("add", "fwmark", mark, "lookup", table, "pref", "100"); err != nil {
		dev.Close()
		return fmt.Errorf("fwmark rule: %w", err)
	}
	// the outer endpoint packets must never enter the tunnel
	if t.server != "" {
		ipRule("del", "to", t.server+"/32", "lookup", "main")
		if err := ipRule("add", "to", t.server+"/32", "lookup", "main", "pref", "40"); err != nil {
			log.Printf("exit %s: endpoint host rule: %v", t.name, err)
		}
	}
	log.Printf("exit %s: tunnel up on %s (%s, tunnel ip %s, mtu %d)", t.name, ifName, t.def.Transport, resp.TunnelIP, mtu)
	return nil
}

// watch probes the tunnel until it dies or teardown is requested.
// Returns true when the tunnel needs a rebuild.
func (m *exitManager) watch(t *exitTunnel) bool {
	fails := 0
	for {
		select {
		case <-t.stop:
			return false
		case <-time.After(watchdogPeriod):
		}
		hs := t.lastHandshake()
		fresh := hs > 0 && time.Since(time.Unix(hs, 0)) < 3*time.Minute
		if fresh {
			if fails > 0 {
				log.Printf("exit %s: handshake recovered", t.name)
			}
			fails = 0
			continue
		}
		fails++
		log.Printf("exit %s: no recent handshake (%d/%d)", t.name, fails, watchdogMaxDown)
		if fails >= watchdogMaxDown {
			return true
		}
	}
}

func (m *exitManager) cleanupDevice(t *exitTunnel) {
	mark := strconv.Itoa(fwMarkBase + t.idx)
	table := strconv.Itoa(ruleTableBase + t.idx)
	ipRule("del", "fwmark", mark, "lookup", table)
	if t.dev != nil {
		t.dev.Close()
		t.dev = nil
		t.td = nil
	}
}

// CloseAll tears down every tunnel (agent shutdown).
func (m *exitManager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tuns {
		m.teardownLocked(t)
	}
}

// MarkForExit returns the SO_MARK value selecting the exit's table, or
// -1 when the exit has no live tunnel.
func (m *exitManager) MarkForExit(name string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tuns[name]
	if !ok || t.dev == nil {
		return -1
	}
	return fwMarkBase + t.idx
}

// DomainConnectAddr returns the in-tunnel domain-connect address for an
// exit (its NIC's server address), ok=false when the tunnel is down.
func (m *exitManager) DomainConnectAddr(name string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tuns[name]
	if !ok || t.dev == nil || t.tunIP == "" {
		return "", false
	}
	addr := exit.DomainConnectAddr(t.tunIP)
	return addr, addr != ""
}

func ipRule(args ...string) error {
	out, err := exec.Command("ip", append([]string{"rule"}, args...)...).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "File exists") && !strings.Contains(string(out), "No such file") {
		return fmt.Errorf("ip rule %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func ipRoute(args ...string) error {
	out, err := exec.Command("ip", append([]string{"route"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip route %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

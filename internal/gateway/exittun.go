package gateway

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/huangzheng2016/pvm/internal/exit"
	"github.com/huangzheng2016/pvm/internal/fsutil"
	"github.com/huangzheng2016/pvm/internal/gw/rules"
	"github.com/huangzheng2016/pvm/internal/tunnel"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
)

type ExitDef struct {
	Provider     string `json:"provider,omitempty"`
	Server       string `json:"server"`
	Token        string `json:"token"`
	Transport    string `json:"transport"`
	Fingerprint  string `json:"fingerprint"`
	WGPrivateKey string `json:"wg_private_key,omitempty"`
	ServerPubKey string `json:"server_pubkey,omitempty"`
	TunnelIP     string `json:"tunnel_ip,omitempty"`
	MTU          int    `json:"mtu,omitempty"`
	Endpoint     string `json:"endpoint,omitempty"`
	DNS          string `json:"dns,omitempty"`
}

const (
	rulePrefBypass   = 50 // direct CIDRs / direct-domain IPs -> main table
	rulePrefVMExit   = 101
	fwMarkBase       = 0x99 // exit idx i uses mark fwMarkBase+i, table 100+i
	ruleTableBase    = 100
	exitPeerKeyDir   = "/etc/pvm/exit-keys"
	exitProbeTimeout = 5 * time.Second
	watchdogPeriod   = 10 * time.Second
	watchdogMaxDown  = 2 // consecutive failed probes before re-enroll
)

var exitProbeDial = func(ctx context.Context, network, address string, mark int) (net.Conn, error) {
	d := net.Dialer{Timeout: exitProbeTimeout, Control: soMark(mark)}
	return d.DialContext(ctx, network, address)
}

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

func tunnelMSS(def ExitDef) int {
	mtu := def.MTU
	if mtu == 0 {
		mtu = exit.MTUFor(def.Transport)
	}
	return mtu - 40
}

func loadOrCreatePeerKey(dir, name string) (string, error) {
	if !exit.ValidName(name) {
		return "", fmt.Errorf("invalid exit name %q", name)
	}
	path := filepath.Join(dir, name+".key")
	if b, err := os.ReadFile(path); err == nil {
		key := strings.TrimSpace(string(b))
		if _, err := tunnel.PublicKey(key); err != nil {
			return "", fmt.Errorf("parse saved peer key %s: %w", path, err)
		}
		return key, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	key, err := tunnel.GenerateKey()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := fsutil.AtomicWrite(path, []byte(key+"\n"), 0o600); err != nil {
		return "", err
	}
	return key, nil
}

// exitTunnel is one live WireGuard client device to an exit-node.
type exitTunnel struct {
	name   string
	def    ExitDef
	idx    int
	tunIP  string
	priv   string // wg private key, persisted across re-enrolls so the server keeps seeing the same peer
	dev    *device.Device
	td     tun.Device
	stop   chan struct{}
	done   chan struct{}
	server string // data-plane endpoint host (for the anti-loop host rule)
	// serverTunIP is the tunnel-side server address from the enroll
	// response (domain-connect/speed target); it cannot be derived from
	// tunIP because the tunnel subnet is a /16.
	serverTunIP string
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
	mu   sync.Mutex
	tuns map[string]*exitTunnel
	// desired ip-rule state (reconciled on Sync)
	vmRules      map[string]int  // vmIP -> exit idx
	vmDirect     map[string]bool // vmIP -> lookup main (cluster-default override)
	cidrRules    map[string]int  // proxied cidr -> exit idx
	cluster      string          // cluster cidr with a default exit, "" = none
	clusterTable string          // its lookup table, re-added on index drift
	clusterLocal string          // cluster cidr pinned to main (replies, vm-to-vm)
	bypass       map[string]bool // direct cidrs/ips -> lookup main
	// learned holds runtime-learned bypass /32s indexed by the domain
	// that produced them (domain -> ip -> last seen). Entries survive
	// config syncs so cached direct flows are never rerouted mid-TTL,
	// but they are evicted when their domain stops being a direct rule
	// (e.g. direct -> proxy) or when they go unused past learnedTTL.
	learned         map[string]map[string]time.Time
	learnedTTL      time.Duration
	learnedFullOnce sync.Once
	// onBypass/onEvict fire when a bypass ip is learned/expired at
	// runtime, so the nft bypass set stays in sync between full rebuilds.
	onBypass func(ip string)
	onEvict  func(ip string)
}

const (
	learnedBypassCap  = 4096
	defaultLearnedTTL = 24 * time.Hour
)

func newExitManager() *exitManager {
	return &exitManager{
		tuns:       map[string]*exitTunnel{},
		vmRules:    map[string]int{},
		vmDirect:   map[string]bool{},
		cidrRules:  map[string]int{},
		bypass:     map[string]bool{},
		learned:    map[string]map[string]time.Time{},
		learnedTTL: defaultLearnedTTL,
	}
}

// Sync converges tunnels and policy routing. With a cluster default exit,
// VMs without an explicit exit use it, while explicitly direct VMs get a
// main-table override.
func (m *exitManager) Sync(exits map[string]ExitDef, vmExits map[string]string, vmIPs map[string]string, proxiedCIDRs, directCIDRs []string, clusterCIDR, defaultExit string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	names := make([]string, 0, len(exits))
	for n := range exits {
		names = append(names, n)
	}

	// tear down tunnels no longer referenced
	for name, t := range m.tuns {
		if _, ok := exits[name]; !ok {
			m.teardownLocked(t, true)
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
	wantDirect := map[string]bool{}
	for vm, exitName := range vmExits {
		ip := vmIPs[vm]
		if ip == "" {
			continue
		}
		if exitName == "direct" {
			wantDirect[ip] = true
			continue
		}
		if exitName == "" {
			continue
		}
		if idx := exitIndex(names, exitName); idx >= 0 {
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
	defIdx := -1
	if defaultExit != "" {
		defIdx = exitIndex(names, defaultExit)
	}
	wantCluster := ""
	if defIdx >= 0 {
		wantCluster = clusterCIDR
	}
	wantBypass := map[string]bool{}
	// The slirp uplink subnet is infrastructure (port-map DNAT answers,
	// gateway-local services, upstream DNS); without this, replies from
	// exit-routed VMs to host-forwarded connections are policy-routed
	// into the tunnel and every port map breaks the moment the VM gets
	// an exit.
	wantBypass["10.0.2.0/24"] = true
	for _, c := range directCIDRs {
		wantBypass[c] = true
	}
	for _, t := range m.tuns {
		if t.server != "" {
			wantBypass[t.server+"/32"] = true
		}
	}
	// runtime-learned bypass IPs survive config syncs; dropping them
	// mid-TTL reroutes cached direct flows into the tunnel.
	for _, ips := range m.learned {
		for ip := range ips {
			wantBypass[ip] = true
		}
	}

	m.reconcileRulesLocked(wantVM, wantCIDR, wantBypass)
	m.reconcileDefaultLocked(wantDirect, wantCluster, defIdx, vmIPs)

	// Cluster-local destinations (gateway replies, vm-to-vm) must never
	// enter a tunnel table: the from-rules above match gateway-local
	// sources too, and tunnel tables carry only a default route.
	if m.clusterLocal != clusterCIDR {
		if m.clusterLocal != "" {
			ipRuleCmd("del", "to", m.clusterLocal, "lookup", "main")
		}
		m.clusterLocal = ""
		if clusterCIDR != "" {
			if err := ipRuleCmd("add", "to", clusterCIDR, "lookup", "main", "pref", "45"); err == nil {
				m.clusterLocal = clusterCIDR
			}
		}
	}
}

// AddBypassIP records a freshly resolved direct/internal-domain IP so
// its traffic skips the tunnel even for proxied VMs. Entries are keyed
// by the domain that produced them (for rule-change eviction) and carry
// a last-seen timestamp (for TTL expiry); both are refreshed on every
// hit. The table is capped and new entries are dropped (fail-safe to
// the tunnel) once full.
func (m *exitManager) AddBypassIP(domain, ip string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	ips, ok := m.learned[domain]
	if !ok {
		ips = map[string]time.Time{}
		m.learned[domain] = ips
	}
	cidr := ip + "/32"
	if _, dup := ips[cidr]; dup {
		ips[cidr] = now
		return
	}
	total := 0
	for _, set := range m.learned {
		total += len(set)
	}
	if total >= learnedBypassCap {
		m.learnedFullOnce.Do(func() {
			log.Printf("exit: learned bypass table full (%d entries), dropping new ones", learnedBypassCap)
		})
		return
	}
	if err := ipRuleCmd("add", "to", cidr, "lookup", "main", "pref", strconv.Itoa(rulePrefBypass)); err == nil {
		ips[cidr] = now
		m.bypass[cidr] = true
		if m.onBypass != nil {
			m.onBypass(cidr)
		}
	}
}

// evictLearnedIP removes one learned /32 everywhere: ip rule, state,
// and (via onEvict) the nft bypass set.
func (m *exitManager) evictLearnedIPLocked(domain, cidr string) {
	ipRuleCmd("del", "to", cidr, "lookup", "main")
	delete(m.learned[domain], cidr)
	if len(m.learned[domain]) == 0 {
		delete(m.learned, domain)
	}
	delete(m.bypass, cidr)
	if m.onEvict != nil {
		m.onEvict(cidr)
	}
}

// ReconcileLearned evicts learned bypass entries whose domain no longer
// resolves as a direct/internal rule — e.g. after the rule flipped to
// proxy, the previously learned IPs must not keep bypassing the tunnel.
func (m *exitManager) ReconcileLearned(eng *rules.Engine) {
	if eng == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for domain := range m.learned {
		d, ok := eng.Match(domain, nil)
		if ok && (d.Action == rules.Direct || d.Internal) {
			continue
		}
		for cidr := range m.learned[domain] {
			m.evictLearnedIPLocked(domain, cidr)
		}
	}
}

// SweepLearned evicts learned bypass entries unused for longer than the
// configured TTL. Called periodically from the agent run loop.
func (m *exitManager) SweepLearned(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for domain, ips := range m.learned {
		for cidr, seen := range ips {
			if now.Sub(seen) > m.learnedTTL {
				m.evictLearnedIPLocked(domain, cidr)
			}
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
			if err := ipRuleCmd("del", "from", ip+"/32", "lookup", strconv.Itoa(ruleTableBase+idx)); err == nil {
				delete(m.vmRules, ip)
			}
		}
	}
	for ip, idx := range wantVM {
		if cur, ok := m.vmRules[ip]; !ok || cur != idx {
			ipRuleCmd("del", "from", ip+"/32", "lookup", strconv.Itoa(ruleTableBase+cur))
			if err := ipRuleCmd("add", "from", ip+"/32", "lookup", strconv.Itoa(ruleTableBase+idx), "pref", strconv.Itoa(rulePrefVMExit)); err == nil {
				m.vmRules[ip] = idx
			}
		}
	}
	for cidr, idx := range m.cidrRules {
		if _, ok := wantCIDR[cidr]; !ok {
			if err := ipRuleCmd("del", "to", cidr, "lookup", strconv.Itoa(ruleTableBase+idx)); err == nil {
				delete(m.cidrRules, cidr)
			}
		}
	}
	for cidr, idx := range wantCIDR {
		if cur, ok := m.cidrRules[cidr]; !ok || cur != idx {
			ipRuleCmd("del", "to", cidr, "lookup", strconv.Itoa(ruleTableBase+cur))
			if err := ipRuleCmd("add", "to", cidr, "lookup", strconv.Itoa(ruleTableBase+idx), "pref", "60"); err == nil {
				m.cidrRules[cidr] = idx
			}
		}
	}
	for cidr := range m.bypass {
		if !wantBypass[cidr] {
			if err := ipRuleCmd("del", "to", cidr, "lookup", "main"); err == nil {
				delete(m.bypass, cidr)
			}
		}
	}
	for cidr := range wantBypass {
		if !m.bypass[cidr] {
			if err := ipRuleCmd("add", "to", cidr, "lookup", "main", "pref", strconv.Itoa(rulePrefBypass)); err == nil {
				m.bypass[cidr] = true
			}
		}
	}
}

// reconcileDefaultLocked converges the cluster-default-exit rules: with
// a default exit set, the whole cluster CIDR selects its table at
// pref 103, and every explicitly direct VM overrides with lookup-main at
// pref 102 — explicit per-VM exits at pref 101 always win. With no default
// exit both rule sets are removed.
func (m *exitManager) reconcileDefaultLocked(wantDirect map[string]bool, wantCluster string, defIdx int, vmIPs map[string]string) {
	for _, ip := range vmIPs {
		if !wantDirect[ip] && !m.vmDirect[ip] {
			// Also clear stale main overrides written by older versions;
			// the in-memory cache alone cannot discover them after a restart.
			ipRuleCmd("del", "from", ip+"/32", "lookup", "main", "pref", "102")
		}
	}
	for ip := range m.vmDirect {
		if !wantDirect[ip] {
			if err := ipRuleCmd("del", "from", ip+"/32", "lookup", "main"); err == nil {
				delete(m.vmDirect, ip)
			}
		}
	}
	for ip := range wantDirect {
		if !m.vmDirect[ip] {
			if err := ipRuleCmd("add", "from", ip+"/32", "lookup", "main", "pref", "102"); err == nil {
				m.vmDirect[ip] = true
			}
		}
	}
	wantTable := ""
	if defIdx >= 0 {
		wantTable = strconv.Itoa(ruleTableBase + defIdx)
	}
	if m.cluster != "" && (wantCluster == "" || m.cluster != wantCluster || m.clusterTable != wantTable) {
		// delete by prefix: the old rule may point at a different table
		// after exits re-sorted, and ip rule tolerates the no-match case.
		ipRuleCmd("del", "from", m.cluster, "pref", "103")
		m.cluster, m.clusterTable = "", ""
	}
	if wantCluster != "" && m.cluster == "" {
		if err := ipRuleCmd("add", "from", wantCluster, "lookup", wantTable, "pref", "103"); err == nil {
			m.cluster, m.clusterTable = wantCluster, wantTable
		}
	}
}

func (m *exitManager) ensureLocked(name string, def ExitDef, idx int) {
	if t, ok := m.tuns[name]; ok {
		if t.def == def && t.idx == idx {
			return // already up with the same parameters and index
		}
		// def or index drifted (an exit was added/removed and re-sorted
		// the indexes): rebuild so ifname/mark/table match the rules.
		m.teardownLocked(t, !sameExitPeer(t.def, def))
	}
	t := &exitTunnel{name: name, def: def, idx: idx, stop: make(chan struct{}), done: make(chan struct{})}
	m.tuns[name] = t
	go m.runTunnel(t)
}

func sameExitPeer(a, b ExitDef) bool {
	return a.Provider == b.Provider &&
		a.Server == b.Server &&
		a.Token == b.Token &&
		a.Transport == b.Transport &&
		a.Fingerprint == b.Fingerprint &&
		a.WGPrivateKey == b.WGPrivateKey &&
		a.ServerPubKey == b.ServerPubKey &&
		a.TunnelIP == b.TunnelIP &&
		a.MTU == b.MTU &&
		a.Endpoint == b.Endpoint &&
		a.DNS == b.DNS
}

func (m *exitManager) teardownLocked(t *exitTunnel, revoke bool) {
	close(t.stop)
	<-t.done
	delete(m.tuns, t.name)
	if revoke && t.def.Provider != exit.ProviderWARP && t.tunIP != "" {
		cl := exit.NewClient(t.def.Server, t.def.Token, t.def.Fingerprint)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := cl.DeletePeer(ctx, t.tunIP); err != nil {
			log.Printf("exit %s: revoke peer %s: %v", t.name, t.tunIP, err)
		}
		cancel()
	}
}

// runTunnel maintains the tunnel until torn down and rebuilds it after
// prolonged handshake failure.
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
			log.Printf("exit %s: watchdog detected dead tunnel, rebuilding", t.name)
		}
		m.cleanupDevice(t)
	}
}

func (m *exitManager) upOnce(t *exitTunnel) error {
	type enrollment struct {
		serverPubKey string
		tunnelIP     string
		serverIP     string
		mtu          int
		endpoint     string
	}
	var resp enrollment
	if t.def.Provider == exit.ProviderWARP {
		t.priv = t.def.WGPrivateKey
		resp = enrollment{
			serverPubKey: t.def.ServerPubKey,
			tunnelIP:     t.def.TunnelIP,
			mtu:          t.def.MTU,
			endpoint:     t.def.Endpoint,
		}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if t.priv == "" {
			priv, err := loadOrCreatePeerKey(exitPeerKeyDir, t.name)
			if err != nil {
				return err
			}
			t.priv = priv
		}
		pub, err := tunnel.PublicKey(t.priv)
		if err != nil {
			return err
		}
		cl := exit.NewClient(t.def.Server, t.def.Token, t.def.Fingerprint)
		r, err := cl.Enroll(ctx, pub, "pvm-gw", t.def.Transport)
		if err != nil {
			return fmt.Errorf("enroll: %w", err)
		}
		resp = enrollment{serverPubKey: r.ServerPubKey, tunnelIP: r.TunnelIP, serverIP: r.ServerIP, mtu: r.MTU, endpoint: r.Endpoint}
	}
	mtu := resp.mtu
	if mtu == 0 {
		mtu = exit.MTUFor(t.def.Transport)
	}
	resolved, server, err := exit.ResolveEndpoint(resp.endpoint)
	if err != nil {
		return fmt.Errorf("resolve endpoint: %w", err)
	}
	resp.endpoint, t.server = resolved, server
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
	privHex, err := tunnel.Hex(t.priv)
	if err != nil {
		dev.Close()
		return err
	}
	pubHex, err := tunnel.Hex(resp.serverPubKey)
	if err != nil {
		dev.Close()
		return err
	}
	conf := fmt.Sprintf(`private_key=%s
public_key=%s
endpoint=%s
allowed_ip=0.0.0.0/0
persistent_keepalive_interval=25
`, privHex, pubHex, resp.endpoint)
	if err := dev.IpcSet(conf); err != nil {
		dev.Close()
		return fmt.Errorf("configure device: %w", err)
	}
	if err := exit.ConfigureTUN(ifName, resp.tunnelIP+"/32"); err != nil {
		dev.Close()
		return err
	}
	t.dev, t.td, t.tunIP = dev, td, resp.tunnelIP
	t.serverTunIP = resp.serverIP

	// policy routing: default via this tunnel in its own table; marked
	// flows (fakeproxy proxy dials) select the table by fwmark.
	table := strconv.Itoa(ruleTableBase + t.idx)
	if err := ipRoute("replace", "default", "dev", ifName, "table", table); err != nil {
		dev.Close()
		return err
	}
	mark := strconv.Itoa(fwMarkBase + t.idx)
	ipRuleCmd("del", "fwmark", mark, "lookup", table) // dedupe, ignore error
	if err := ipRuleCmd("add", "fwmark", mark, "lookup", table, "pref", "100"); err != nil {
		dev.Close()
		return fmt.Errorf("fwmark rule: %w", err)
	}
	// the outer endpoint packets must never enter the tunnel
	if t.server != "" {
		ipRuleCmd("del", "to", t.server+"/32", "lookup", "main")
		if err := ipRuleCmd("add", "to", t.server+"/32", "lookup", "main", "pref", "40"); err != nil {
			log.Printf("exit %s: endpoint host rule: %v", t.name, err)
		}
	}
	log.Printf("exit %s: tunnel up on %s (%s, tunnel ip %s, mtu %d)", t.name, ifName, t.def.Transport, resp.tunnelIP, mtu)
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
		probeErr := m.probe(t)
		if fresh && probeErr == nil {
			if fails > 0 {
				log.Printf("exit %s: tunnel recovered", t.name)
			}
			fails = 0
			continue
		}
		fails++
		if fresh {
			log.Printf("exit %s: data-plane probe failed (%d/%d): %v", t.name, fails, watchdogMaxDown, probeErr)
		} else {
			log.Printf("exit %s: no recent handshake (%d/%d)", t.name, fails, watchdogMaxDown)
		}
		if fails >= watchdogMaxDown {
			return true
		}
	}
}

func (m *exitManager) ensureTunnelRoute(t *exitTunnel) error {
	table := strconv.Itoa(ruleTableBase + t.idx)
	if err := ipRoute("replace", "default", "dev", tunnelIfName(t.idx), "table", table); err != nil {
		return err
	}
	mark := strconv.Itoa(fwMarkBase + t.idx)
	if err := ipRuleCmd("add", "fwmark", mark, "lookup", table, "pref", "100"); err != nil {
		return err
	}
	if t.server != "" {
		if err := ipRuleCmd("add", "to", t.server+"/32", "lookup", "main", "pref", "40"); err != nil {
			log.Printf("exit %s: endpoint host rule: %v", t.name, err)
		}
	}
	return nil
}

func (m *exitManager) probe(t *exitTunnel) error {
	if err := m.ensureTunnelRoute(t); err != nil {
		return err
	}
	if t.def.Provider == exit.ProviderWARP {
		return nil
	}
	if t.serverTunIP == "" {
		return fmt.Errorf("missing server tunnel address")
	}
	mark := fwMarkBase + t.idx
	addr := net.JoinHostPort(t.serverTunIP, strconv.Itoa(exit.SpeedPort))
	conn, err := exitProbeDial(context.Background(), "tcp4", addr, mark)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(exitProbeTimeout))
	if _, err := fmt.Fprintf(conn, "GET /ping HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", t.serverTunIP); err != nil {
		return err
	}
	status, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.Contains(status, " 200") {
		return fmt.Errorf("probe status: %s", strings.TrimSpace(status))
	}
	if err := m.probeUDP(t); err != nil {
		return err
	}
	return m.probeICMP(t)
}

func (m *exitManager) probeUDP(t *exitTunnel) error {
	mark := fwMarkBase + t.idx
	addr := net.JoinHostPort(t.serverTunIP, strconv.Itoa(exit.UDPProbePort))
	conn, err := exitProbeDial(context.Background(), "udp4", addr, mark)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(exitProbeTimeout))
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	packet := append([]byte("PVMP"), nonce...)
	if _, err := conn.Write(packet); err != nil {
		return err
	}
	resp := make([]byte, len(packet))
	if _, err := io.ReadFull(conn, resp); err != nil {
		return err
	}
	if string(resp) != string(packet) {
		return fmt.Errorf("udp probe response mismatch")
	}
	return nil
}

func (m *exitManager) probeICMP(t *exitTunnel) error {
	conn, err := exitProbeDial(context.Background(), "ip4:icmp", t.serverTunIP, fwMarkBase+t.idx)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(exitProbeTimeout))
	id := uint16(time.Now().UnixNano())
	msg := make([]byte, 12)
	msg[0], msg[1] = 8, 0
	binary.BigEndian.PutUint16(msg[4:6], id)
	binary.BigEndian.PutUint16(msg[6:8], 1)
	copy(msg[8:], "PVM!")
	icmpSetChecksum(msg)
	if _, err := conn.Write(msg); err != nil {
		return err
	}
	resp := make([]byte, 64)
	n, err := conn.Read(resp)
	if err != nil {
		return err
	}
	reply := icmpMessage(resp[:n])
	if len(reply) < 12 || reply[0] != 0 || reply[1] != 0 || binary.BigEndian.Uint16(reply[4:6]) != id || binary.BigEndian.Uint16(reply[6:8]) != 1 || string(reply[8:12]) != "PVM!" {
		return fmt.Errorf("icmp probe response mismatch")
	}
	return nil
}

func (m *exitManager) cleanupDevice(t *exitTunnel) {
	mark := strconv.Itoa(fwMarkBase + t.idx)
	table := strconv.Itoa(ruleTableBase + t.idx)
	ipRuleCmd("del", "fwmark", mark, "lookup", table)
	// drop the anti-loop host rule too; leaving it pins a stale VPS IP
	// to the main table after the tunnel is gone.
	if t.server != "" {
		ipRuleCmd("del", "to", t.server+"/32", "lookup", "main")
	}
	if t.dev != nil {
		t.dev.Close()
		t.dev = nil
		t.td = nil
	}
}

func (m *exitManager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tuns {
		m.teardownLocked(t, false)
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
	if !ok || t.dev == nil || t.serverTunIP == "" {
		return "", false
	}
	addr := exit.DomainConnectAddr(t.serverTunIP)
	return addr, addr != ""
}

// ipRuleCmd is indirection for tests (the gateway only runs on linux,
// but the reconcile logic is testable anywhere with a stub).
var ipRuleCmd = ipRule

func ipRule(args ...string) error {
	out, err := exec.Command("ip", append([]string{"rule"}, args...)...).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "File exists") && !strings.Contains(string(out), "No such file") {
		return fmt.Errorf("ip rule %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

var ipRoute = ipRouteImpl

func ipRouteImpl(args ...string) error {
	out, err := exec.Command("ip", append([]string{"route"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip route %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

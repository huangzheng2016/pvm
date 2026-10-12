// Package gateway implements the in-guest gateway agent: bridge manager,
// fixed-mapping DHCP server, DNS forwarder and nftables NAT/DNAT. It is
// config-driven: the host daemon pushes /etc/pvm/gateway.json and the
// agent converges actual state to it.
package gateway

import (
	"encoding/json"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	gwpolicy "github.com/huangzheng2016/pvm/internal/gw/policy"
	"github.com/huangzheng2016/pvm/internal/gw/rules"
)

// fakeProxyPort receives the fake-ip TCP flows REDIRECTed by nft.
const fakeProxyPort = 7893

type VMEntry struct {
	Name string `json:"name"`
	MAC  string `json:"mac"`
	IP   string `json:"ip"`
}

type MapEntry struct {
	HostPort  int    `json:"host_port"` // pool port on the gateway uplink
	VMIP      string `json:"vm_ip"`
	GuestPort int    `json:"guest_port"`
	Proto     string `json:"proto"` // tcp | udp
}

type Config struct {
	CIDR        string            `json:"cidr"`         // e.g. 100.99.1.0/24
	GwIP        string            `json:"gw_ip"`        // e.g. 100.99.1.1
	DNSUpstream string            `json:"dns_upstream"` // slirp DNS, 10.0.2.3
	VMs         []VMEntry         `json:"vms"`
	Maps        []MapEntry        `json:"maps"`
	Rules       []rules.Rule      `json:"rules,omitempty"`    // routing rules (fake-ip engine)
	VMExits     map[string]string `json:"vm_exits,omitempty"` // vm name -> exit name; ""/absent = direct
	// DefaultExit is the cluster-wide fallback exit ("", absent = direct)
	// used when neither a rule nor a per-VM route matches. Wired through
	// the gateway config but not yet exposed by the daemon/CLI.
	DefaultExit string `json:"default_exit,omitempty"`
	// Exits carries the connection defs for every exit referenced by
	// Rules or VMExits (keyed by exit name).
	Exits map[string]ExitDef `json:"exits,omitempty"`
}

type Agent struct {
	configPath string

	mu    sync.Mutex
	cfg   *Config
	upl   string // uplink interface name, e.g. enp0s1
	br    string
	exits *exitManager
	stop  chan struct{}
}

func New(configPath string) *Agent {
	a := &Agent{configPath: configPath, br: "br0", exits: newExitManager(), stop: make(chan struct{})}
	// learned bypass IPs are inserted into the nft set immediately;
	// evicted ones (rule flip or TTL) are removed the same way; full
	// table rebuilds re-include the survivors via BypassList.
	a.exits.onBypass = func(cidr string) {
		if out, err := exec.Command("nft", "add", "element", "inet", "pvm", "bypass4", "{", cidr, "}").CombinedOutput(); err != nil {
			log.Printf("nft bypass4 add %s: %v (%s)", cidr, err, strings.TrimSpace(string(out)))
		}
	}
	a.exits.onEvict = func(cidr string) {
		if out, err := exec.Command("nft", "delete", "element", "inet", "pvm", "bypass4", "{", cidr, "}").CombinedOutput(); err != nil {
			log.Printf("nft bypass4 del %s: %v (%s)", cidr, err, strings.TrimSpace(string(out)))
		}
	}
	return a
}

func (a *Agent) loadConfig() (*Config, error) {
	data, err := os.ReadFile(a.configPath)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// Run blocks, converging system state to the config every few seconds.
func (a *Agent) Run() error {
	var (
		dhcpSrv *DHCPServer
		fdns    *fakeDNS
		fproxy  *fakeProxy
		lastCfg []byte
	)
	for {
		raw, err := os.ReadFile(a.configPath)
		if err != nil {
			log.Printf("config: %v (retrying)", err)
			time.Sleep(3 * time.Second)
			continue
		}
		if string(raw) != string(lastCfg) {
			var c Config
			if err := json.Unmarshal(raw, &c); err != nil {
				log.Printf("config invalid: %v", err)
				time.Sleep(3 * time.Second)
				continue
			}
			a.mu.Lock()
			a.cfg = &c
			a.mu.Unlock()

			if err := a.applyNetwork(&c); err != nil {
				log.Printf("apply network: %v", err)
			}
			if dhcpSrv == nil {
				dhcpSrv = NewDHCPServer(a.macIPMap(), a.br)
				if err := dhcpSrv.Start(); err != nil {
					log.Printf("dhcp: %v", err)
				}
			} else {
				dhcpSrv.UpdateMap(a.macIPMap())
			}
			if fdns == nil && c.DNSUpstream != "" {
				f, err := newFakeDNS(c.DNSUpstream, func(domain string, ips []string) {
					for _, ip := range ips {
						a.exits.AddBypassIP(domain, ip)
					}
				})
				if err != nil {
					log.Printf("fake dns init: %v", err)
				} else if err := f.Start(c.GwIP); err != nil {
					log.Printf("fake dns listen: %v", err)
					f.Close()
				} else {
					fdns = f
					fdns.RunDumpLoop("/run/pvm/fakeip.json", 3*time.Second)
					fproxy = newFakeProxy(nil, c.DNSUpstream, a.vmNameByIP)
					if err := fproxy.Start(fakeProxyPort); err != nil {
						log.Printf("fake proxy listen: %v", err)
					}
				}
			}
			if fdns != nil {
				if err := fdns.SetRules(c.Rules); err != nil {
					log.Printf("rules: %v", err)
				}
				// a rule that flipped direct -> proxy must not keep its
				// previously learned bypass IPs.
				a.exits.ReconcileLearned(fdns.Rules())
				if fproxy != nil {
					pol := gwpolicy.New(fdns.Rules(), fdns.Pool())
					for name, exit := range c.VMExits {
						if exit != "" && exit != "direct" {
							pol.SetVMExit(name, exit)
						}
					}
					pol.SetDefaultExit(c.DefaultExit)
					fproxy.SetPolicy(pol)
					fproxy.SetExitHooks(a.exits.MarkForExit, a.exits.DomainConnectAddr)
				}
			}
			// exit tunnels + policy routing converge on every apply
			vmIPs := map[string]string{}
			for _, v := range c.VMs {
				vmIPs[v.Name] = v.IP
			}
			var proxiedCIDRs, directCIDRs []string
			for _, r := range c.Rules {
				if r.Type != rules.CIDR {
					continue
				}
				if r.Action == rules.Proxy && r.Exit != "" {
					proxiedCIDRs = append(proxiedCIDRs, r.Pattern+"@"+r.Exit)
				} else {
					directCIDRs = append(directCIDRs, r.Pattern)
				}
			}
			a.exits.Sync(c.Exits, c.VMExits, vmIPs, proxiedCIDRs, directCIDRs, c.CIDR, c.DefaultExit)
			lastCfg = raw
		}
		a.reconcileLinks()
		a.exits.SweepLearned(time.Now())
		select {
		case <-a.stop:
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

// vmNameByIP maps a cluster source IP back to the VM name from config.
func (a *Agent) vmNameByIP(ip net.IP) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cfg == nil {
		return ""
	}
	want := ip.String()
	for _, v := range a.cfg.VMs {
		if v.IP == want {
			return v.Name
		}
	}
	return ""
}

func (a *Agent) macIPMap() map[string]VMEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]VMEntry{}
	if a.cfg != nil {
		for _, v := range a.cfg.VMs {
			out[v.MAC] = v
		}
	}
	return out
}

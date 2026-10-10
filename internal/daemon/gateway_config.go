package daemon

import (
	"encoding/json"
	"path/filepath"

	"github.com/huangzheng2016/pvm/internal/cluster"
	"github.com/huangzheng2016/pvm/internal/exit"
	"github.com/huangzheng2016/pvm/internal/gw/rules"
)

// gatewayConfigFromCluster renders the /etc/pvm/gateway.json payload.
func gatewayConfigFromCluster(s *Server, cl *cluster.Cluster) []byte {
	type vmEntry struct {
		Name string `json:"name"`
		MAC  string `json:"mac"`
		IP   string `json:"ip"`
	}
	type mapEntry struct {
		HostPort  int    `json:"host_port"`
		VMIP      string `json:"vm_ip"`
		GuestPort int    `json:"guest_port"`
		Proto     string `json:"proto"`
	}
	type exitDef struct {
		Server      string `json:"server"`
		Token       string `json:"token"`
		Transport   string `json:"transport"`
		Fingerprint string `json:"fingerprint"`
	}
	doc := struct {
		CIDR        string             `json:"cidr"`
		GwIP        string             `json:"gw_ip"`
		DNSUpstream string             `json:"dns_upstream"`
		VMs         []vmEntry          `json:"vms"`
		Maps        []mapEntry         `json:"maps"`
		Rules       []rules.Rule       `json:"rules,omitempty"`
		VMExits     map[string]string  `json:"vm_exits,omitempty"`
		Exits       map[string]exitDef `json:"exits,omitempty"`
	}{
		CIDR: cl.CIDR, GwIP: cl.GwIP, DNSUpstream: "10.0.2.3",
		Rules: cl.Rules, VMExits: cl.VMExits,
	}
	for _, v := range cl.VMs {
		doc.VMs = append(doc.VMs, vmEntry{Name: v.Name, MAC: v.MAC, IP: v.IP})
	}
	for _, m := range cl.Maps {
		doc.Maps = append(doc.Maps, mapEntry{HostPort: m.PoolPort, VMIP: m.VMIP, GuestPort: m.GuestPort, Proto: m.Proto})
	}
	// only exits referenced by a rule or a per-VM route are pushed
	want := map[string]bool{}
	for _, exitName := range cl.VMExits {
		if exitName != "" && exitName != "direct" {
			want[exitName] = true
		}
	}
	for _, r := range cl.Rules {
		if r.Action == rules.Proxy && r.Exit != "" {
			want[r.Exit] = true
		}
	}
	for name := range want {
		e, err := exit.LoadEntry(filepath.Join(s.P.Home, "exits"), name)
		if err != nil {
			continue // unregistered exit: the gateway marks flows un routable
		}
		if doc.Exits == nil {
			doc.Exits = map[string]exitDef{}
		}
		doc.Exits[name] = exitDef{Server: e.Server, Token: e.Token, Transport: e.Transport, Fingerprint: e.Fingerprint}
	}
	data, _ := json.MarshalIndent(doc, "", "  ")
	return data
}

package daemon

import (
	"encoding/json"

	"github.com/huangzheng2016/pvm/internal/cluster"
)

// gatewayConfigFromCluster renders the /etc/pvm/gateway.json payload.
func gatewayConfigFromCluster(cl *cluster.Cluster) []byte {
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
	doc := struct {
		CIDR        string     `json:"cidr"`
		GwIP        string     `json:"gw_ip"`
		DNSUpstream string     `json:"dns_upstream"`
		VMs         []vmEntry  `json:"vms"`
		Maps        []mapEntry `json:"maps"`
	}{
		CIDR: cl.CIDR, GwIP: cl.GwIP, DNSUpstream: "10.0.2.3",
	}
	for _, v := range cl.VMs {
		doc.VMs = append(doc.VMs, vmEntry{Name: v.Name, MAC: v.MAC, IP: v.IP})
	}
	for _, m := range cl.Maps {
		doc.Maps = append(doc.Maps, mapEntry{HostPort: m.PoolPort, VMIP: m.VMIP, GuestPort: m.GuestPort, Proto: m.Proto})
	}
	data, _ := json.MarshalIndent(doc, "", "  ")
	return data
}

// Package netutil holds the shared non-public IPv4 destination policy used
// by the VPS exit, gateway direct path and active egress checks.
package netutil

import "net"

var blockedEgressCIDRs = []string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
	"169.254.0.0/16", "172.16.0.0/12", "192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/3",
}

var blockedEgressNetworks = func() []*net.IPNet {
	var out []*net.IPNet
	for _, cidr := range blockedEgressCIDRs {
		_, n, _ := net.ParseCIDR(cidr)
		out = append(out, n)
	}
	return out
}()

// IsBlockedEgressIP reports whether ip is not a public unicast IPv4 target:
// private, loopback, link-local, CGNAT, benchmark, multicast or reserved.
func IsBlockedEgressIP(ip net.IP) bool {
	if ip.To4() == nil {
		return true
	}
	for _, n := range blockedEgressNetworks {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func BlockedEgressCIDRs() []string {
	return append([]string(nil), blockedEgressCIDRs...)
}

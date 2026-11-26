package exit

import (
	"net"

	"github.com/huangzheng2016/pvm/internal/netutil"
)

var blockedEgressCIDRs = netutil.BlockedEgressCIDRs()

func blockedEgressIP(ip net.IP) bool { return netutil.IsBlockedEgressIP(ip) }

// IsBlockedEgressIP reports whether ip is not a public unicast IPv4 target:
// private, loopback, link-local, CGNAT, benchmark, multicast or reserved.
func IsBlockedEgressIP(ip net.IP) bool { return netutil.IsBlockedEgressIP(ip) }

// BlockedEgressCIDRs returns the non-public IPv4 ranges blocked by default.
func BlockedEgressCIDRs() []string { return netutil.BlockedEgressCIDRs() }

func egressAllowed(ip net.IP, allow []*net.IPNet, localIPs ...net.IP) bool {
	if ip.To4() == nil {
		return false
	}
	for _, n := range allow {
		if n.Contains(ip) {
			return true
		}
	}
	for _, local := range localIPs {
		if local.Equal(ip) {
			return false
		}
	}
	return !blockedEgressIP(ip)
}

func tunnelServerIP(cidr string) string {
	ip, _, err := net.ParseCIDR(cidr)
	if err != nil || ip.To4() == nil {
		return ""
	}
	v4 := ip.To4()
	v4[3]++
	return v4.String()
}

// Snapshot local IPv4 addresses at startup; packet forwarding never enumerates interfaces.
func localEgressIPs() ([]net.IP, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var ips []net.IP
	for _, addr := range addrs {
		if n, ok := addr.(*net.IPNet); ok && n.IP.To4() != nil {
			ips = append(ips, append(net.IP(nil), n.IP.To4()...))
		}
	}
	return ips, nil
}

package exit

import "net"

// Keep every data plane on the same destination policy, including IP literals.
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

func blockedEgressIP(ip net.IP) bool {
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

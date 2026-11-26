package gateway

import (
	"fmt"
	"net"

	"github.com/huangzheng2016/pvm/internal/exit"
	gwpolicy "github.com/huangzheng2016/pvm/internal/gw/policy"
)

// checkDirectTarget keeps relay-originated direct flows off the host LAN,
// slirp host loopback (10.0.2.2) and link-local services unless an explicit
// internal-domain rule or direct CIDR rule opted in.
func checkDirectTarget(pol *gwpolicy.Engine, domain string, ip net.IP) error {
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("direct target %s is not IPv4", ip)
	}
	if !exit.IsBlockedEgressIP(ip) {
		return nil
	}
	if pol.AllowsPrivateDirect(domain, ip) {
		return nil
	}
	return fmt.Errorf("direct target %s (%s) is a private or reserved address; add an explicit `pvm route add direct --cidr` or internal rule to allow it", domain, ip)
}

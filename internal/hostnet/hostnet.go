// Package hostnet selects the host's physical IPv4 egress interface (avoiding
// TUN/VPN/bridge loopbacks) and binds outbound sockets to it.
package hostnet

import (
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Interface struct {
	Name  string
	Index int
	IPv4  string
}

var virtualIfaceRE = regexp.MustCompile(`^(?:utun|bridge|vmen|vmenet|vmnet|tap|tun|wg|pvm|lima|awdl|llw|p2p|gif|stf|anpi|ap|veth|docker|br-|tailscale)[0-9a-zA-Z_.-]*$`)

func IsVirtual(name string) bool {
	return name == "lo" || name == "lo0" || virtualIfaceRE.MatchString(name)
}

var (
	cacheMu  sync.Mutex
	cached   *Interface
	cachedAt time.Time
)

// SelectPhysical returns the preferred physical IPv4 interface, cached for
// 30s so per-dial checks stay cheap. Returns nil when none is available or
// PVM_NO_BIND_IF=1 is set.
func SelectPhysical() *Interface {
	if os.Getenv("PVM_NO_BIND_IF") == "1" {
		return nil
	}
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if time.Since(cachedAt) < 30*time.Second {
		return cached
	}
	ifc, err := selectPhysical()
	if err != nil || ifc == nil {
		cached = nil
		cachedAt = time.Now()
		return nil
	}
	if ni, err := net.InterfaceByName(ifc.Name); err == nil {
		ifc.Index = ni.Index
	}
	cached = ifc
	cachedAt = time.Now()
	return cached
}

// BindDialer pins d to the selected physical interface when one is available.
func BindDialer(d *net.Dialer) {
	if d == nil {
		return
	}
	ifc := SelectPhysical()
	if ifc == nil || ifc.Index <= 0 {
		return
	}
	d.Control = bindControl(ifc.Name, ifc.Index)
}

func skipBindForAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && (ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified())
}

package exit

import (
	"fmt"
	"os/exec"

	"golang.zx2c4.com/wireguard/tun"
)

// NAT backend names for --nat.
const (
	NATAuto      = "auto"
	NATNftables  = "nft"
	NATIptables  = "iptables"
	NATUserspace = "userspace"
)

// cmdExists is exec.LookPath, stubbed by tests.
var cmdExists = func(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// natBackend decides how decrypted WireGuard packets reach the internet.
type natBackend interface {
	name() string
	// newTun creates the tun.Device for one WireGuard transport.
	newTun(tunName string, mtu int) (tun.Device, error)
	// setup runs after all transports exist; nil tunNames when userspace.
	setup(tunNames []string, cidrs []string) error
	// cleanup removes whatever setup installed, scoped to this instance's
	// tun names and cidrs so coexisting instances are not affected.
	cleanup(tunNames []string, cidrs []string) error
}

func resolveNAT(choice string) (natBackend, error) {
	switch choice {
	case "", NATAuto:
		if cmdExists("nft") {
			return nftBackend{}, nil
		}
		if cmdExists("iptables") {
			return iptablesBackend{}, nil
		}
		return newUserspaceBackend(), nil
	case NATNftables:
		if !cmdExists("nft") {
			return nil, fmt.Errorf("--nat=nft but nft command not found")
		}
		return nftBackend{}, nil
	case NATIptables:
		if !cmdExists("iptables") {
			return nil, fmt.Errorf("--nat=iptables but iptables command not found")
		}
		return iptablesBackend{}, nil
	case NATUserspace:
		return newUserspaceBackend(), nil
	}
	return nil, fmt.Errorf("unknown --nat %q (auto|nft|iptables|userspace)", choice)
}

func netfilterTun(tunName string, mtu int) (tun.Device, error) {
	return tun.CreateTUN(tunName, mtu)
}

func ResolveNATName(choice string) (string, error) {
	b, err := resolveNAT(choice)
	if err != nil {
		return "", err
	}
	return b.name(), nil
}

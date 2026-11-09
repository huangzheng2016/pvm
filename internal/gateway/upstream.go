package gateway

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	gwdns "github.com/huangzheng2016/pvm/internal/gw/dns"
)

func dnsUpstreamAddress(raw string) (network, addr string, err error) {
	network = "udp"
	if after, ok := strings.CutPrefix(raw, "tcp://"); ok {
		network, raw = "tcp", after
	} else if strings.Contains(raw, "://") {
		return "", "", fmt.Errorf("unsupported DNS upstream scheme in %q", raw)
	}
	if raw == "" {
		return "", "", fmt.Errorf("DNS upstream is empty")
	}
	if _, _, err := net.SplitHostPort(raw); err != nil {
		if net.ParseIP(raw) == nil && strings.Contains(raw, ":") {
			return "", "", fmt.Errorf("invalid DNS upstream %q", raw)
		}
		raw = net.JoinHostPort(raw, "53")
	}
	return network, raw, nil
}

func dnsUpstream(raw string) (gwdns.Upstream, error) {
	network, addr, err := dnsUpstreamAddress(raw)
	if err != nil {
		return nil, err
	}
	if network == "tcp" {
		return gwdns.TCPUpstream{Addr: addr}, nil
	}
	return gwdns.UDPUpstream{Addr: addr}, nil
}

func upstreamResolver(upstream string) *net.Resolver {
	network, addr, err := dnsUpstreamAddress(upstream)
	if err != nil {
		return nil
	}
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		d := net.Dialer{Timeout: 5 * time.Second}
		return d.DialContext(ctx, network, addr)
	}}
}

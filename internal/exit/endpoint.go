package exit

import (
	"context"
	"fmt"
	"net"
	"time"
)

func ResolveEndpoint(endpoint string) (string, string, error) {
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil || host == "" || port == "" {
		return "", "", fmt.Errorf("invalid endpoint %q", endpoint)
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.To4() == nil {
			return "", "", fmt.Errorf("endpoint %q is not IPv4", endpoint)
		}
		return endpoint, host, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", host)
	if err != nil {
		return "", "", fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(ips) == 0 {
		return "", "", fmt.Errorf("resolve %s: no IPv4 addresses", host)
	}
	resolved := ips[0].String()
	return net.JoinHostPort(resolved, port), resolved, nil
}

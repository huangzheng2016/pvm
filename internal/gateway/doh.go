package gateway

import "strings"

// wellKnownDoHResolvers lists the public DoH/DoQ resolver IPs that
// hard-coded-DoH applications try directly. Domain routing rules only
// apply to DNS that passes the gateway resolver; an app carrying its
// own DoH endpoint would otherwise bypass them entirely. Port 53 to
// these IPs is already hijacked onto the gateway resolver, so blocking
// 443/853 does not break legitimate name lookups — it only forces the
// DoH/DoQ path to fail so clients fall back to plain DNS.
//
// Sources (2026-10): cloudflare.com/learning/dns, dns.google,
// quad9.net, opendns.com, adguard-dns.io, cleanbrowsing.org, dns.sb.
var wellKnownDoHResolvers = []string{
	// Cloudflare (1.1.1.1 family, incl. malware/family variants)
	"1.1.1.1", "1.0.0.1", "1.1.1.2", "1.0.0.2", "1.1.1.3", "1.0.0.3",
	// Google Public DNS
	"8.8.8.8", "8.8.4.4",
	// Quad9
	"9.9.9.9", "149.112.112.112", "9.9.9.10", "149.112.112.10",
	"9.9.9.11", "149.112.112.11",
	// OpenDNS / Cisco Umbrella
	"208.67.222.222", "208.67.220.220", "208.67.222.220", "208.67.220.222",
	// AdGuard DNS
	"94.140.14.14", "94.140.15.15", "94.140.14.15", "94.140.15.16",
	// CleanBrowsing
	"185.228.168.9", "185.228.169.9", "185.228.168.10", "185.228.169.11",
	// dns.sb
	"185.222.222.222", "45.11.45.11",
}

func dohResolverSet() string {
	return "{ " + strings.Join(wellKnownDoHResolvers, ", ") + " }"
}

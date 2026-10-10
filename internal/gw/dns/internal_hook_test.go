package dns

import (
	"testing"

	mdns "github.com/miekg/dns"

	"github.com/huangzheng2016/pvm/internal/gw/rules"
)

// Internal-rule answers must surface through OnInternalAnswer (both the
// default-uplink and designated-resolver paths) so the routing layer can
// learn bypass IPs; non-matching names must never trigger it.
func TestOnInternalAnswerHook(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dnsAddr string
	}{
		{"default uplink", ""},
		{"designated resolver", "192.0.2.53:53"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, up, eng := newTestServer(t)
			if err := eng.Add(rules.Rule{Type: rules.DomainSuffix, Pattern: "corp.test", Action: rules.Internal, DNS: tc.dnsAddr}); err != nil {
				t.Fatal(err)
			}
			var designatedSeen string
			srv.NewInternalUpstream = func(addr string) Upstream {
				designatedSeen = addr
				return up
			}
			var gotName string
			var gotIPs []string
			srv.OnInternalAnswer = func(name string, resp *mdns.Msg) {
				gotName = name
				for _, rr := range resp.Answer {
					if a, ok := rr.(*mdns.A); ok {
						gotIPs = append(gotIPs, a.A.String())
					}
				}
			}

			query(t, srv, "www.corp.test", mdns.TypeA)

			if gotName != "www.corp.test" {
				t.Fatalf("hook name = %q", gotName)
			}
			if len(gotIPs) != 1 || gotIPs[0] != "93.184.216.34" {
				t.Fatalf("hook ips = %v", gotIPs)
			}
			if tc.dnsAddr != "" && designatedSeen != tc.dnsAddr {
				t.Fatalf("designated resolver = %q, want %q", designatedSeen, tc.dnsAddr)
			}

			gotName = ""
			query(t, srv, "www.other.com", mdns.TypeA)
			if gotName != "" {
				t.Fatal("hook fired for a fake-ip (non-internal) name")
			}
		})
	}
}

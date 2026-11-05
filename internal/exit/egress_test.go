package exit

import (
	"net"
	"strings"
	"testing"
	"time"
)

func TestEgressPolicySharedAcrossProtocols(t *testing.T) {
	_, allow, _ := net.ParseCIDR("10.0.0.0/24")
	for _, tc := range []struct {
		ip   string
		want bool
	}{
		{"10.0.0.1", true}, {"10.0.1.1", false}, {"127.0.0.1", false},
		{"169.254.169.254", false}, {"100.64.0.1", false}, {"198.18.0.1", false},
		{"203.0.113.1", true}, {"::1", false}, {"::ffff:127.0.0.1", false},
	} {
		ip := net.ParseIP(tc.ip)
		if got := egressAllowed(ip, []*net.IPNet{allow}); got != tc.want {
			t.Errorf("%s allowed=%v", tc.ip, got)
		}
		_, err := newDCServer([]*net.IPNet{allow}).pickResolveIP([]net.IP{ip})
		if (err == nil) != tc.want {
			t.Errorf("domain resolve policy differs for %s", tc.ip)
		}
	}
}

func TestNetfilterInputOnlyAllowsServices(t *testing.T) {
	names, cidrs := []string{"pvmexit0"}, []string{"10.99.0.0/16"}
	nft := nftRuleset(names, cidrs)
	input := strings.Split(strings.Split(nft, "chain pvm_input {")[1], "chain pvm_forward")[0]
	if strings.Contains(input, "10.0.0.0/8") {
		t.Fatal("egress allow-list opened VPS input")
	}
	for _, line := range strings.Split(input, "\n") {
		if strings.Contains(line, " accept") && strings.Contains(line, "iifname") && !strings.Contains(line, "dport") && !strings.Contains(line, "icmp type echo-request") {
			t.Fatalf("unrestricted input accept: %s", line)
		}
	}
	if !strings.Contains(input, "iifname \"pvmexit0\" drop") {
		t.Fatal("missing input drop")
	}
	for _, rule := range iptablesRules(names, cidrs) {
		if rule[1] != "INPUT" {
			continue
		}
		spec := rule[2]
		if strings.Contains(spec, "10.0.0.0/8") {
			t.Fatal("allow-list opened iptables input")
		}
		if strings.Contains(spec, "-j ACCEPT") && !strings.Contains(spec, "--dport") && !strings.Contains(spec, "--icmp-type echo-request") {
			t.Fatalf("unrestricted input: %s", spec)
		}
	}
}

func TestUDPSessionLiteralPeerQuota(t *testing.T) {
	tab := &udpSessionTable{m: map[udpFlow]*udpSession{}, closed: make(chan struct{})}
	defer tab.closeAll()
	for i := 0; i < maxFlowsPerPeer; i++ {
		flow := udpFlow{srcIP: "203.0.113.1", srcPort: uint16(i)}
		if !tab.putIfAbsent(flow, &udpSession{}) {
			t.Fatalf("slot %d refused", i)
		}
	}
	overflow := udpFlow{srcIP: "203.0.113.1", srcPort: maxFlowsPerPeer}
	if tab.putIfAbsent(overflow, &udpSession{}) {
		t.Fatal("peer quota exceeded")
	}
	other := udpFlow{srcIP: "203.0.113.2"}
	if !tab.putIfAbsent(other, &udpSession{}) {
		t.Fatal("one peer blocked another")
	}
	first := udpFlow{srcIP: "203.0.113.1"}
	tab.remove(first, tab.get(first))
	if !tab.putIfAbsent(overflow, &udpSession{}) {
		t.Fatal("released slot not reused")
	}
	tab.closeAll()
	if len(tab.peers) != 0 {
		t.Fatal("peer accounting leaked")
	}
}

func TestPendingFlowPeerQuota(t *testing.T) {
	u := &userspaceBackend{}
	var releases []func()
	for i := 0; i < maxFlowsPerPeer; i++ {
		release, ok := u.reservePeerFlow("tcp", "203.0.113.1")
		if !ok {
			t.Fatal("premature rejection")
		}
		releases = append(releases, release)
	}
	if _, ok := u.reservePeerFlow("tcp", "203.0.113.1"); ok {
		t.Fatal("pending dial quota exceeded")
	}
	release, ok := u.reservePeerFlow("tcp", "203.0.113.2")
	if !ok {
		t.Fatal("other peer rejected")
	}
	release()
	for _, release := range releases {
		release()
	}
	if len(u.peerFlows) != 0 || u.flowCount["tcp"] != 0 {
		t.Fatal("quota leaked")
	}
}

func TestIdleRelayClosesBothDirections(t *testing.T) {
	client, a := net.Pipe()
	b, upstream := net.Pipe()
	defer client.Close()
	defer upstream.Close()
	done := make(chan struct{})
	go func() {
		proxyTCP(&idleTimeoutConn{Conn: a, d: 20 * time.Millisecond}, &idleTimeoutConn{Conn: b, d: 20 * time.Millisecond})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("idle relay leaked")
	}
}

func TestSuggestedTimezoneExplicitAndInvalid(t *testing.T) {
	cfg := ServerConfig{Token: "YOUR_TOKEN", StateDir: t.TempDir(), NAT: NATUserspace, SuggestedTZ: "Etc/UTC"}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if s.sugTZ != "Etc/UTC" {
		t.Fatalf("timezone=%s", s.sugTZ)
	}
	s.Close()
	cfg.SuggestedTZ = "invalid/zone"
	if s, err := NewServer(cfg); err == nil {
		s.Close()
		t.Fatal("invalid timezone accepted")
	}
}

func TestLocalEgressRequiresExplicitAllow(t *testing.T) {
	local := net.ParseIP("203.0.113.1")
	_, allow, _ := net.ParseCIDR("203.0.113.1/32")
	if egressAllowed(local, nil, local) {
		t.Fatal("public local interface allowed by default")
	}
	if _, err := newDCServer(nil, local).pickResolveIP([]net.IP{local}); err == nil {
		t.Fatal("domain path allowed local interface")
	}
	if !egressAllowed(local, []*net.IPNet{allow}, local) {
		t.Fatal("explicit local allow refused")
	}
	names, cidrs := []string{"pvmexit0"}, []string{"10.99.0.0/16"}
	nft := nftRuleset(names, cidrs, allow)
	if !strings.Contains(nft, `iifname "pvmexit0" ip daddr 203.0.113.1/32 accept`) {
		t.Fatal("missing scoped local allow")
	}
	found := false
	for _, r := range iptablesRules(names, cidrs, allow) {
		if r[1] == "INPUT" && strings.Contains(r[2], "-d 203.0.113.1/32") {
			found = true
			if !strings.Contains(r[2], "-i pvmexit0 ") {
				t.Fatal("unscoped local allow")
			}
		}
	}
	if !found {
		t.Fatal("missing iptables local allow")
	}
}

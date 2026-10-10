package gateway

import (
	"encoding/binary"
	"net"
	"strings"
	"testing"
)

func discoverPacket(xid uint32, mac string) []byte {
	b := make([]byte, 300)
	b[0], b[1], b[2] = 1, 1, 6
	binary.BigEndian.PutUint32(b[4:8], xid)
	binary.BigEndian.PutUint16(b[10:12], 0x8000) // broadcast flag
	copy(b[28:34], mustParseMAC(mac))
	copy(b[236:240], []byte{99, 130, 83, 99})
	b[240], b[241], b[242], b[243] = 53, 1, 1, 255 // opt53 DISCOVER, end
	return b
}

func TestDHCPRoundTrip(t *testing.T) {
	mac := "52:54:00:01:00:0a"
	req, err := parseDHCP(discoverPacket(0xdeadbeef, mac))
	if err != nil {
		t.Fatal(err)
	}
	if req.xid != 0xdeadbeef || req.mac != mac || req.msgType != 1 {
		t.Errorf("parsed: %+v", req)
	}
	yip := net.ParseIP("192.168.1.10").To4()
	gw := net.ParseIP("192.168.1.1").To4()
	resp := buildDHCPReply(req, 2, yip, gw, net.CIDRMask(24, 32), gw, "dev1")
	// parse our own reply back
	if resp[0] != 2 {
		t.Errorf("op: %d", resp[0])
	}
	if binary.BigEndian.Uint32(resp[4:8]) != 0xdeadbeef {
		t.Error("xid mismatch")
	}
	if !net.IP(resp[16:20]).Equal(yip) {
		t.Errorf("yiaddr: %v", resp[16:20])
	}
	// options: expect 53=OFFER, 3=gw, 6=gw, 12=dev1
	var gotType, gotHostname byte
	var gotRouter []byte
	for i := 240; i+2 <= len(resp); {
		code, ln := resp[i], int(resp[i+1])
		if code == 255 {
			break
		}
		switch code {
		case 53:
			gotType = resp[i+2]
		case 3:
			gotRouter = resp[i+2 : i+2+ln]
		case 12:
			gotHostname = byte(len(resp[i+2 : i+2+ln]))
			if string(resp[i+2:i+2+ln]) != "dev1" {
				t.Errorf("hostname: %q", resp[i+2:i+2+ln])
			}
		}
		i += 2 + ln
	}
	if gotType != 2 {
		t.Errorf("msg type: %d", gotType)
	}
	if !net.IP(gotRouter).Equal(gw) {
		t.Errorf("router: %v", gotRouter)
	}
	if gotHostname != 4 {
		t.Errorf("hostname len: %d", gotHostname)
	}
}

func TestDHCPUnknownMACIgnored(t *testing.T) {
	s := NewDHCPServer(map[string]VMEntry{}, "br0")
	req, err := parseDHCP(discoverPacket(1, "52:54:00:01:00:0a"))
	if err != nil {
		t.Fatal(err)
	}
	s.mu.RLock()
	_, known := s.byMAC[req.mac]
	s.mu.RUnlock()
	if known {
		t.Error("unknown mac marked known")
	}
}

func TestRenderNFT(t *testing.T) {
	cfg := &Config{
		CIDR: "192.168.1.0/24", GwIP: "192.168.1.1",
		Maps: []MapEntry{
			{HostPort: 30000, VMIP: "192.168.1.10", GuestPort: 80, Proto: "tcp"},
			{HostPort: 30053, VMIP: "192.168.1.11", GuestPort: 53, Proto: "udp"},
		},
	}
	out := RenderNFT(cfg, "enp0s1", "br0", nil)
	for _, want := range []string{
		"destroy table inet pvm",
		`oifname "enp0s1" masquerade`,
		`iifname "enp0s1" tcp dport 30000 dnat ip to 192.168.1.10:80`,
		`iifname "enp0s1" udp dport 30053 dnat ip to 192.168.1.11:53`,
		"type nat hook prerouting",
		"type nat hook postrouting",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ruleset missing %q\n%s", want, out)
		}
	}
}

// The forward chain is default-deny for br0 -> uplink: bypass
// destinations pass for every VM (or direct rules are dead for proxied
// VMs), direct VMs pass on their registered DHCP MAC/IP pair, and
// exit-routed VMs match neither. The bypass accept must precede the
// final drop.
func TestRenderNFTBypassBeforeDrop(t *testing.T) {
	cfg := &Config{
		CIDR: "192.168.1.0/24", GwIP: "192.168.1.1",
		VMs:     []VMEntry{{Name: "dev1", MAC: "52:54:00:00:00:10", IP: "192.168.1.10"}},
		VMExits: map[string]string{"dev1": "us"},
	}
	out := RenderNFT(cfg, "enp0s1", "br0", []string{"10.0.0.0/8", "104.20.23.154/32"})
	for _, want := range []string{
		"set bypass4 {",
		"elements = { 10.0.0.0/8, 104.20.23.154/32 }",
		`iifname "br0" oifname "enp0s1" ip daddr @bypass4 accept`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ruleset missing %q\n%s", want, out)
		}
	}
	accept := strings.Index(out, `ip daddr @bypass4 accept`)
	drop := strings.Index(out, `iifname "br0" oifname "enp0s1" drop`)
	if accept < 0 || drop < 0 {
		t.Fatalf("missing bypass accept or final drop\n%s", out)
	}
	if accept > drop {
		t.Errorf("bypass accept must precede the final drop\n%s", out)
	}
	// exit-routed VMs get no registered-pair accept at all
	if strings.Contains(out, "52:54:00:00:00:10") {
		t.Errorf("exit-routed VM must not get a registered-pair accept\n%s", out)
	}
	// sets are only legal at table level: bypass4 must be declared
	// before the first chain or nft -f rejects the whole render
	setIdx := strings.Index(out, "set bypass4 {")
	chainIdx := strings.Index(out, "chain postrouting {")
	if setIdx < 0 || chainIdx < 0 || setIdx > chainIdx {
		t.Errorf("set bypass4 must be declared at table level before chains\n%s", out)
	}
}

// Direct VMs keep uplink access via their registered DHCP MAC/IP pair;
// the default-deny drop closes everything else (spoofed/unregistered).
func TestRenderNFTDirectVMRegisteredPair(t *testing.T) {
	cfg := &Config{
		CIDR: "192.168.1.0/24", GwIP: "192.168.1.1",
		VMs: []VMEntry{
			{Name: "dev1", MAC: "52:54:00:00:00:10", IP: "192.168.1.10"},
			{Name: "dev2", MAC: "52:54:00:00:00:11", IP: "192.168.1.11"},
		},
		VMExits: map[string]string{"dev2": "us"},
	}
	out := RenderNFT(cfg, "enp0s1", "br0", nil)
	if !strings.Contains(out, `iifname "br0" ether saddr 52:54:00:00:00:10 ip saddr 192.168.1.10 oifname "enp0s1" accept`) {
		t.Errorf("direct VM missing registered-pair accept\n%s", out)
	}
	if strings.Contains(out, "52:54:00:00:00:11") {
		t.Errorf("exit-routed VM must not get a registered-pair accept\n%s", out)
	}
	if !strings.Contains(out, `iifname "br0" oifname "enp0s1" drop`) {
		t.Errorf("default-deny drop missing\n%s", out)
	}
	// Port-map (DNAT) answers return to the slirp infrastructure subnet
	// and must pass for every VM, including exit-routed ones.
	slirp := `iifname "br0" oifname "enp0s1" ip daddr 10.0.2.0/24 accept`
	if !strings.Contains(out, slirp) {
		t.Errorf("slirp infrastructure accept missing\n%s", out)
	}
	if strings.Index(out, slirp) > strings.Index(out, `iifname "br0" oifname "enp0s1" drop`) {
		t.Errorf("slirp accept must come before the default drop\n%s", out)
	}
}

// DoH/DoQ to well-known public resolvers is rejected on 443 while
// plain DNS (:53) to the same IPs is still hijacked onto the gateway.
func TestRenderNFTDoHBlock(t *testing.T) {
	cfg := &Config{CIDR: "192.168.1.0/24", GwIP: "192.168.1.1"}
	out := RenderNFT(cfg, "enp0s1", "br0", nil)
	for _, ip := range []string{"1.1.1.1", "8.8.8.8", "9.9.9.9"} {
		if !strings.Contains(out, ip) {
			t.Errorf("resolver %s missing from DoH block set\n%s", ip, out)
		}
	}
	if !strings.Contains(out, `tcp dport 443 reject`) || !strings.Contains(out, `udp dport 443 reject`) {
		t.Errorf("DoH rejects for 443 missing\n%s", out)
	}
	if !strings.Contains(out, `udp dport 53 dnat`) {
		t.Errorf("plain-DNS hijack must remain\n%s", out)
	}
}

// IPv6 never reaches the uplink (the cluster and kill-switch are v4).
func TestRenderNFTIPv6Drop(t *testing.T) {
	cfg := &Config{CIDR: "192.168.1.0/24", GwIP: "192.168.1.1"}
	out := RenderNFT(cfg, "enp0s1", "br0", nil)
	if !strings.Contains(out, `iifname "br0" oifname "enp0s1" meta nfproto ipv6 drop`) {
		t.Errorf("ipv6 uplink drop missing\n%s", out)
	}
}

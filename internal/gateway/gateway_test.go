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
	out := RenderNFT(cfg, "enp0s1", "br0")
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

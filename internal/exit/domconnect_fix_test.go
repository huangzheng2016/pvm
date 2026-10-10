package exit

import (
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestBlockedEgressIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.53.0.9", "169.254.169.254", "169.254.1.1",
		"10.0.0.1", "10.99.0.1", "172.16.0.1", "172.31.255.1",
		"192.168.1.1", "0.0.0.0", "224.0.0.1", "255.255.255.255",
		"::1", "fe80::1", "2001:db8::1",
	}
	for _, s := range blocked {
		if !blockedEgressIP(net.ParseIP(s)) {
			t.Errorf("%s must be blocked", s)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "203.0.113.10", "203.0.113.9"}
	for _, s := range allowed {
		if blockedEgressIP(net.ParseIP(s)) {
			t.Errorf("%s must not be blocked", s)
		}
	}
}

func TestValidConnectDomain(t *testing.T) {
	bad := []string{
		"", "127.0.0.1", "169.254.169.254", "::1", "10.0.0.1",
		"evil..com", "-lead.com", "trail-.com", strings.Repeat("a", 64) + ".com",
		"semi;colon.com", "new\nline.com", "space .com", "SLASH/x.com",
		strings.Repeat("a", 254),
	}
	for _, d := range bad {
		if validConnectDomain(d) {
			t.Errorf("%q must be rejected", d)
		}
	}
	good := []string{"example.com", "a-b_c.example.co.uk", "xn--nxasmq6b.example", "host.", "1.2.3.4.5.in-addr.arpa."}
	for _, d := range good {
		if !validConnectDomain(d) {
			t.Errorf("%q must be accepted", d)
		}
	}
}

// dcServer refusing an IP-literal domain end to end.
func TestDomainConnectRejectsIPLiteral(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	dc := newDCServer(nil)
	dialed := make(chan string, 1)
	dc.dial = func(network, address string) (net.Conn, error) {
		dialed <- address
		return nil, io.ErrClosedPipe
	}
	go dc.serve(ln)

	conn, err := net.DialTimeout("tcp", ln.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	err = WriteDomainConnect(conn, "169.254.169.254", 80)
	if err == nil || !strings.Contains(err.Error(), "exit refused") {
		t.Fatalf("want refusal for metadata IP, got %v", err)
	}
	select {
	case <-dialed:
		t.Fatal("dialer ran for a blocked domain")
	case <-time.After(300 * time.Millisecond):
	}
}

// restrictedDial blocks private destinations but honors the allowlist.
func TestRestrictedDialPolicy(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	target := echo.Addr().String()

	dc := newDCServer(nil)
	if _, err := dc.restrictedDial("tcp", target); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("loopback dial must be refused, got %v", err)
	}

	_, allowNet, _ := net.ParseCIDR("127.0.0.0/8")
	dc2 := newDCServer([]*net.IPNet{allowNet})
	c, err := dc2.restrictedDial("tcp", target)
	if err != nil {
		t.Fatalf("allowlisted loopback dial: %v", err)
	}
	c.Close()
}

// Per-peer relay cap.
func TestDCServerPeerCap(t *testing.T) {
	dc := newDCServer(nil)
	for i := 0; i < dcMaxPerPeer; i++ {
		if !dc.enter("10.99.0.2") {
			t.Fatalf("enter %d below cap refused", i)
		}
	}
	if dc.enter("10.99.0.2") {
		t.Fatal("enter beyond cap accepted")
	}
	if !dc.enter("10.99.0.3") {
		t.Fatal("cap is per-peer")
	}
	dc.leave("10.99.0.2")
	if !dc.enter("10.99.0.2") {
		t.Fatal("leave did not free a slot")
	}
}

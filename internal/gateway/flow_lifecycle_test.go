package gateway

import (
	"net"
	"testing"
)

func TestOldUDPFlowCannotCloseReplacement(t *testing.T) {
	p := newFakeUDP(nil, "10.0.2.3", nil)
	a, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	old := &fakeUDPFlow{up: a}
	replacement := &fakeUDPFlow{up: b}
	p.flows[old.key] = replacement
	p.closeFlow(old)
	if p.flows[old.key] != replacement {
		t.Fatal("old flow removed replacement")
	}
	if _, err := b.WriteToUDP([]byte("ping"), b.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatalf("replacement closed: %v", err)
	}
}

func TestOldICMPFlowCannotCloseReplacement(t *testing.T) {
	p := newFakeICMP(nil, "10.0.2.3", nil)
	a, peerA := net.Pipe()
	defer a.Close()
	defer peerA.Close()
	b, peerB := net.Pipe()
	defer b.Close()
	defer peerB.Close()
	old := &fakeICMPFlow{up: a}
	replacement := &fakeICMPFlow{up: b}
	p.flows[old.key] = replacement
	p.closeFlow(old)
	if p.flows[old.key] != replacement {
		t.Fatal("old flow removed replacement")
	}
	assertOpen(t, b, "old flow closed replacement")
}

func TestICMPIdentifierWrapSkipsLiveFlows(t *testing.T) {
	p := newFakeICMP(nil, "10.0.2.3", nil)
	p.nextUpID = 0xffff
	p.flows[fakeICMPKey{id: 1}] = &fakeICMPFlow{upID: 1}
	p.flows[fakeICMPKey{id: 2}] = &fakeICMPFlow{upID: 2}
	if got := p.allocUpID(); got != 3 {
		t.Fatalf("identifier = %d, want 3", got)
	}
}

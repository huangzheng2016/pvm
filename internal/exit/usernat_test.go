package exit

import (
	"sync"
	"testing"
	"time"
)

func TestUDPSessionTableReaper(t *testing.T) {
	tab := newUDPSessionTable(50 * time.Millisecond)
	tab.interval = 20 * time.Millisecond
	flow := udpFlow{srcIP: "10.99.0.2", srcPort: 1000, dstIP: "1.1.1.1", dstPort: 53}
	tab.put(flow, &udpSession{flow: flow, lastActive: time.Now()})

	var wg sync.WaitGroup
	done := make(chan struct{})
	go func() { tab.reaper(&wg); close(done) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if tab.get(flow) == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if tab.get(flow) != nil {
		t.Fatal("idle session not reaped")
	}
	tab.closeAll()
	wg.Wait()
	<-done
}

func TestUDPSessionTableCloseAll(t *testing.T) {
	tab := newUDPSessionTable(time.Hour)
	tab.put(udpFlow{srcIP: "a"}, &udpSession{lastActive: time.Now()})
	tab.put(udpFlow{srcIP: "b"}, &udpSession{lastActive: time.Now()})
	tab.closeAll()
	if tab.get(udpFlow{srcIP: "a"}) != nil || tab.get(udpFlow{srcIP: "b"}) != nil {
		t.Fatal("sessions not cleared")
	}
	tab.closeAll()
}

func TestChanTUNReadWrite(t *testing.T) {
	ct := newChanTUN("test0", 1420)
	// WG device Write (decrypted packet) -> inbound
	pkt := []byte("ip packet")
	n, err := ct.Write([][]byte{append(make([]byte, 4), pkt...)}, 4)
	if err != nil || n != 1 {
		t.Fatalf("write: n=%d err=%v", n, err)
	}
	select {
	case got := <-ct.inbound:
		if string(got) != string(pkt) {
			t.Fatalf("inbound = %q", got)
		}
	default:
		t.Fatal("packet not queued inbound")
	}

	// outbound -> Read (packets to encrypt)
	ct.outbound <- pkt
	bufs := [][]byte{make([]byte, 2048)}
	sizes := []int{0}
	nr, err := ct.Read(bufs, sizes, 4)
	if err != nil || nr != 1 || sizes[0] != len(pkt) {
		t.Fatalf("read: nr=%d size=%d err=%v", nr, sizes[0], err)
	}

	ct.Close()
	if _, err := ct.Read(bufs, sizes, 4); err == nil {
		t.Fatal("read after close should fail")
	}
	ct.Close() // idempotent
}

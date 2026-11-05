package tunnel

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/tun/tuntest"
)

func shortTCPDeadlines(t *testing.T, auth, idle time.Duration) {
	t.Helper()
	oldAuth, oldIdle := tcpFirstReadTimeoutNs.Load(), tcpIdleTimeoutNs.Load()
	tcpFirstReadTimeoutNs.Store(int64(auth))
	tcpIdleTimeoutNs.Store(int64(idle))
	t.Cleanup(func() { tcpFirstReadTimeoutNs.Store(oldAuth); tcpIdleTimeoutNs.Store(oldIdle) })
}

func waitTCPCount(t *testing.T, b *TCPBind, want int) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		n := len(b.tcpConnMap)
		b.mu.Unlock()
		if n == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("connection count did not become %d", want)
}

func TestTCPUnauthenticatedFramesExpire(t *testing.T) {
	shortTCPDeadlines(t, 100*time.Millisecond, time.Second)
	for _, mode := range []string{"zero", "garbage", "fake_handshake", "slow", "queue_full"} {
		t.Run(mode, func(t *testing.T) {
			b, addr := openTestBind(t)
			c, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			done := make(chan struct{})
			go func() {
				defer close(done)
				data := []byte{99}
				if mode == "fake_handshake" {
					data = make([]byte, 148)
					data[0] = 1
				}
				if mode == "zero" {
					data = nil
				}
				var h [4]byte
				binary.LittleEndian.PutUint32(h[:], uint32(len(data)))
				frame := append(h[:], data...)
				for {
					if mode == "slow" {
						frame = []byte{1}
					}
					if _, err := c.Write(frame); err != nil {
						return
					}
					if mode != "queue_full" {
						time.Sleep(5 * time.Millisecond)
					}
				}
			}()
			c.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, err = c.Read(make([]byte, 1))
			if err == nil {
				t.Fatal("connection remained open")
			}
			if e, ok := err.(net.Error); ok && e.Timeout() {
				t.Fatalf("server did not close: %v", err)
			}
			c.Close()
			<-done
			waitTCPCount(t, b, 0)
		})
	}
}

func TestTCPWireGuardKeepaliveAndReconnect(t *testing.T) {
	shortTCPDeadlines(t, 2*time.Second, 1500*time.Millisecond)
	a, b := NewTCPBind().(*TCPBind), NewTCPBind().(*TCPBind)
	ta, tb := tuntest.NewChannelTUN(), tuntest.NewChannelTUN()
	da, err := NewDevice(ta.TUN(), a, false)
	if err != nil {
		t.Fatal(err)
	}
	defer da.Close()
	db, err := NewDevice(tb.TUN(), b, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ka, _ := GenerateKey()
	kb, _ := GenerateKey()
	pa, _ := PublicKey(ka)
	pb, _ := PublicKey(kb)
	kha, _ := Hex(ka)
	khb, _ := Hex(kb)
	pha, _ := Hex(pa)
	phb, _ := Hex(pb)
	b.mu.Lock()
	port := b.listener.Addr().(*net.TCPAddr).Port
	b.mu.Unlock()
	if err := db.IpcSet(fmt.Sprintf("private_key=%s\npublic_key=%s\nallowed_ip=203.0.113.1/32\npersistent_keepalive_interval=1\n", khb, pha)); err != nil {
		t.Fatal(err)
	}
	if err := da.IpcSet(fmt.Sprintf("private_key=%s\npublic_key=%s\nendpoint=127.0.0.1:%d\nallowed_ip=203.0.113.2/32\npersistent_keepalive_interval=1\n", kha, phb, port)); err != nil {
		t.Fatal(err)
	}
	ping := func() {
		t.Helper()
		packet := tuntest.Ping(netip.MustParseAddr("203.0.113.2"), netip.MustParseAddr("203.0.113.1"))
		select {
		case ta.Outbound <- packet:
		case <-time.After(4 * time.Second):
			t.Fatal("TUN write timeout")
		}
		select {
		case <-tb.Inbound:
		case <-time.After(4 * time.Second):
			t.Fatal("authenticated tunnel did not deliver ping")
		}
	}
	ping()
	a.mu.Lock()
	var original *tcpConn
	for _, c := range a.tcpConnMap {
		original = c
	}
	a.mu.Unlock()
	time.Sleep(3200 * time.Millisecond)
	ping()
	a.mu.Lock()
	var current *tcpConn
	for _, c := range a.tcpConnMap {
		current = c
	}
	a.mu.Unlock()
	if original == nil || original != current {
		t.Fatal("keepalives failed to retain authenticated connection")
	}
	if err := da.IpcSet("public_key=" + phb + "\npersistent_keepalive_interval=0\n"); err != nil {
		t.Fatal(err)
	}
	if err := db.IpcSet("public_key=" + pha + "\npersistent_keepalive_interval=0\n"); err != nil {
		t.Fatal(err)
	}
	waitTCPCount(t, a, 0)
	waitTCPCount(t, b, 0)
	ping()
}

func TestTCPAuthenticationRequiresCurrentReceivedEndpoint(t *testing.T) {
	shortTCPDeadlines(t, 200*time.Millisecond, time.Second)
	b, addr := openTestBind(t)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.Write([]byte{1, 0, 0, 0, 99}); err != nil {
		t.Fatal(err)
	}
	var received *recvData
	select {
	case received = <-b.recvChan:
	case <-time.After(time.Second):
		t.Fatal("receive timeout")
	}
	ep := received.endpoint.(*tcpEndpoint)
	b.dataPool.Put(received)
	// A device cookie reply is stateless and cannot authenticate this stream.
	if err := b.Send([][]byte{{3, 0, 0, 0}}, ep); err != nil {
		t.Fatal(err)
	}
	// An endpoint retained from another stream cannot transfer authentication.
	stale := &tcpEndpoint{Endpoint: ep.Endpoint, entry: &tcpConn{}}
	if err := b.Send([][]byte{{2, 0, 0, 0}}, stale); err != nil {
		t.Fatal(err)
	}
	ep.entry.deadlineMu.Lock()
	authenticated := ep.entry.authenticated
	ep.entry.deadlineMu.Unlock()
	if authenticated {
		t.Fatal("cookie or stale endpoint authenticated a connection")
	}
	waitTCPCount(t, b, 0)
}

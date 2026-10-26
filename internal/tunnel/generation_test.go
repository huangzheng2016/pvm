package tunnel

import (
	"errors"
	"net"
	"testing"

	"golang.zx2c4.com/wireguard/conn"
)

func TestTCPBindReceiveStaysWithGeneration(t *testing.T) {
	b := NewTCPBind().(*TCPBind)
	defer b.Close()
	for range 40 {
		old, _, err := b.Open(0)
		if err != nil {
			t.Fatal(err)
		}
		if err := b.Close(); err != nil {
			t.Fatal(err)
		}
		_, _, err = b.Open(0)
		if err != nil {
			t.Fatal(err)
		}
		data := b.dataPool.Get().(*recvData)
		data.size = 1
		data.buff[0] = 42
		b.recvChan <- data
		n, err := old[0]([][]byte{make([]byte, MaxSegmentSize)}, make([]int, 1), make([]conn.Endpoint, 1))
		if n != 0 || !errors.Is(err, net.ErrClosed) {
			t.Fatalf("old receiver consumed new generation: n=%d err=%v", n, err)
		}
		select {
		case got := <-b.recvChan:
			if got != data {
				t.Fatal("new packet changed")
			}
			b.dataPool.Put(got)
		default:
			t.Fatal("new generation packet lost")
		}
		b.Close()
	}
}

func TestTCPBindRejectsDoubleOpen(t *testing.T) {
	b, _ := openTestBind(t)
	if _, _, err := b.Open(0); !errors.Is(err, conn.ErrBindAlreadyOpen) {
		t.Fatalf("second Open: %v", err)
	}
}

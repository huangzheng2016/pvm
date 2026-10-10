package gateway

import (
	"net"
	"testing"
	"time"
)

func TestCopyWithIdleTimeout(t *testing.T) {
	a, b := net.Pipe()
	done := make(chan struct{})
	go func() {
		copyWithIdleTimeout(a, b, 25*time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("idle connection was not closed by deadline")
	}
	a.Close()
	b.Close()
}

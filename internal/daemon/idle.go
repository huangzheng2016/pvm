package daemon

import (
	"net"
	"time"
)

const relayIdleTimeout = 30 * time.Minute

// copyWithIdleTimeout forwards bytes while applying read/write deadlines
// to every operation, so half-dead connections cannot leak goroutines and
// file descriptors forever.
func copyWithIdleTimeout(to, from net.Conn, timeout time.Duration) {
	buf := make([]byte, 32*1024)
	for {
		_ = from.SetReadDeadline(time.Now().Add(timeout))
		n, err := from.Read(buf)
		if n > 0 {
			_ = to.SetWriteDeadline(time.Now().Add(timeout))
			if _, writeErr := to.Write(buf[:n]); writeErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

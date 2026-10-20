//go:build race

package exit

import "time"

// Under -race the in-process gVisor + wireguard path runs ~10x slower;
// relax internal deadlines so full-package runs stay deterministic.
func init() {
	handshakeTimeout = 2 * time.Minute
	httpTimeout = 2 * time.Minute
	probeHandshakeTimeout = 2 * time.Minute
	clientHTTPTimeout = 2 * time.Minute
	udpProbeTimeout = 2 * time.Minute
	icmpProbeTimeout = 2 * time.Minute
	testOpTimeout = 2 * time.Minute
	testLongTimeout = 4 * time.Minute
	testProbeTimeout = 10 * time.Second
}

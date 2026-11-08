//go:build windows

package gateway

import (
	"fmt"
	"net"
	"syscall"
)

func dhcpSocketControl(bindDev string) func(network, address string, c syscall.RawConn) error {
	return nil
}

func setsockoptMark(fd uintptr, mark int) error {
	return fmt.Errorf("SO_MARK unsupported on windows")
}

func getsockoptOriginalDst(fd uintptr) (*net.TCPAddr, error) {
	return nil, fmt.Errorf("SO_ORIGINAL_DST unsupported on windows")
}

func setsockoptIPInt(fd uintptr, opt, val int) error {
	return fmt.Errorf("IP socket option unsupported on windows")
}

func parseOriginalDstUDP(oob []byte) (*net.UDPAddr, error) {
	return nil, fmt.Errorf("IP_ORIGDSTADDR unsupported on windows")
}

//go:build !windows

package gateway

import (
	"fmt"
	"net"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func dhcpSocketControl(bindDev string) func(network, address string, c syscall.RawConn) error {
	return func(_, _ string, c syscall.RawConn) error {
		var err error
		c.Control(func(fd uintptr) {
			if err = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1); err != nil {
				return
			}
			// broadcast replies otherwise follow the default route
			// out the uplink and never reach the cluster segment.
			// SO_BINDTODEVICE is linux-only (=25); the agent only
			// ever runs on linux guests, and the constant keeps the
			// package testable on darwin.
			err = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, 25, bindDev)
		})
		return err
	}
}

func setsockoptMark(fd uintptr, mark int) error {
	return unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, soMarkOpt, mark)
}

func getsockoptOriginalDst(fd uintptr) (*net.TCPAddr, error) {
	var mreq [16]byte
	size := uint32(len(mreq))
	_, _, errno := syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd, 0, soOriginalDst,
		uintptr(unsafe.Pointer(&mreq[0])), uintptr(unsafe.Pointer(&size)), 0)
	if errno != 0 {
		return nil, errno
	}
	port := int(mreq[2])<<8 | int(mreq[3])
	return &net.TCPAddr{IP: net.IPv4(mreq[4], mreq[5], mreq[6], mreq[7]), Port: port}, nil
}

func setsockoptIPInt(fd uintptr, opt, val int) error {
	return unix.SetsockoptInt(int(fd), 0, opt, val)
}

func parseOriginalDstUDP(oob []byte) (*net.UDPAddr, error) {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, err
	}
	for i := range msgs {
		m := &msgs[i]
		if int(m.Header.Level) == 0 && int(m.Header.Type) == ipOriginalDst && len(m.Data) >= 16 {
			return &net.UDPAddr{
				IP:   net.IPv4(m.Data[4], m.Data[5], m.Data[6], m.Data[7]),
				Port: int(m.Data[2])<<8 | int(m.Data[3]),
			}, nil
		}
	}
	return nil, fmt.Errorf("original destination control message not found")
}

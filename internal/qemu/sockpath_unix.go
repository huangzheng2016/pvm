//go:build !windows

package qemu

import "golang.org/x/sys/unix"

func maxUnixSocketPathLen() int {
	return len(unix.RawSockaddrUnix{}.Path)
}

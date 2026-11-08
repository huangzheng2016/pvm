//go:build windows

package qemu

func maxUnixSocketPathLen() int {
	return 108
}

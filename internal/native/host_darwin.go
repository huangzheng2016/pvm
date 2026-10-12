//go:build darwin

package native

import "golang.org/x/sys/unix"

// HostMemMB returns the host's physical memory in MiB.
func HostMemMB() int {
	v, err := unix.SysctlUint64("hw.memsize")
	if err != nil || v == 0 {
		return 8192
	}
	return int(v >> 20)
}

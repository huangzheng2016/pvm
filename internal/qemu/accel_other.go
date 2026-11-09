//go:build !windows

package qemu

func windowsHypervisorPlatformEnabled() bool {
	return false
}

//go:build !windows

package qemu

func windowsHypervisorPlatformEnabled() bool {
	return false
}

func whpxQEMU64CPU() string { return "qemu64" }

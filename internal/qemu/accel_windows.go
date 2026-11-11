//go:build windows

package qemu

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var whvGetCapability = windows.NewLazySystemDLL("WinHvPlatform.dll").NewProc("WHvGetCapability")

// windowsHypervisorPlatformEnabled reports whether the Windows Hypervisor
// Platform is enabled and its hypervisor is running. WinHvPlatform.dll ships
// only with that feature; unlike Get-WindowsOptionalFeature, querying it
// needs no elevation, and the installers set pvm up for a standard user.
func windowsHypervisorPlatformEnabled() bool {
	if whvGetCapability.Find() != nil {
		return false
	}
	const whvCapabilityCodeHypervisorPresent = 0
	var present, written uint32
	hr, _, _ := whvGetCapability.Call(whvCapabilityCodeHypervisorPresent,
		uintptr(unsafe.Pointer(&present)), unsafe.Sizeof(present), uintptr(unsafe.Pointer(&written)))
	return hr == 0 && present != 0
}

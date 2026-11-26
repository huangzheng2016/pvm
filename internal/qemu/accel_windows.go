//go:build windows

package qemu

import (
	"sync"
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

var (
	whpxCPUOnce  sync.Once
	whpxCPUModel string
)

// whpxQEMU64CPU adds the SSE4/POPCNT features the hypervisor actually exposes
// to the qemu64 first-boot fallback, so modern toolchains do not SIGILL before
// cloud-init installs noxsave into grub.
func whpxQEMU64CPU() string {
	whpxCPUOnce.Do(func() {
		whpxCPUModel = "qemu64"
		if whvGetCapability.Find() != nil {
			return
		}
		const whvCapabilityCodeProcessorFeatures = 0x1001
		var bits, written uint64
		hr, _, _ := whvGetCapability.Call(whvCapabilityCodeProcessorFeatures,
			uintptr(unsafe.Pointer(&bits)), unsafe.Sizeof(bits), uintptr(unsafe.Pointer(&written)))
		if int32(hr) < 0 || written != uint64(unsafe.Sizeof(bits)) {
			return
		}
		whpxCPUModel = whpxQEMU64Model(bits)
	})
	return whpxCPUModel
}

// Bit positions follow WHV_X64_PROCESSOR_FEATURES in the Windows SDK.
func whpxQEMU64Model(bits uint64) string {
	model := "qemu64"
	for _, f := range []struct {
		mask uint64
		flag string
	}{
		{1 << 2, "+ssse3"},
		{1 << 3, "+sse4.1"},
		{1 << 4, "+sse4.2"},
		{1 << 7, "+popcnt"},
	} {
		if bits&f.mask != 0 {
			model += "," + f.flag
		}
	}
	return model
}

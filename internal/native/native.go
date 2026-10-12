// Package native is the single source of truth for architecture selection.
// pvm intentionally supports only the host's native architecture; there is
// no cross-architecture guest, image, or runtime fallback.
package native

import (
	"fmt"
	"runtime"
)

// Arch is the host's native GOARCH. Every image, VM, and guest binary is
// built for exactly this target.
const Arch = runtime.GOARCH

// QemuArch returns the QEMU system target for the native architecture.
// The two supported native mappings are kept explicit.
func QemuArch() string {
	switch Arch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	default:
		panic(fmt.Sprintf("unsupported native architecture %q", Arch))
	}
}

// GuestBinary returns the filename of a native guest binary. Only the two
// supported roles are accepted so unexpected architectures or payloads
// cannot slip into the packed manifest.
func GuestBinary(kind string) string {
	switch kind {
	case "agent", "gateway":
		return kind + "-linux-" + Arch
	default:
		panic(fmt.Sprintf("unknown guest binary kind %q", kind))
	}
}

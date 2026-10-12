// Package native restricts images, guests, and runtimes to the host architecture.
package native

import (
	"fmt"
	"runtime"
)

const Arch = runtime.GOARCH

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

// GuestBinary only accepts roles allowed in the packed manifest.
func GuestBinary(kind string) string {
	switch kind {
	case "agent", "gateway":
		return kind + "-linux-" + Arch
	default:
		panic(fmt.Sprintf("unknown guest binary kind %q", kind))
	}
}

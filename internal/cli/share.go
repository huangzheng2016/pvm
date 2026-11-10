package cli

import (
	"fmt"
	"strings"
)

func nativeShareMode(requested string, explicit bool, supports9p bool, goos string) (mode, warning string, err error) {
	if requested == "" {
		requested = "private"
	}
	if goos != "windows" || supports9p || requested == "none" {
		return requested, "", nil
	}
	if explicit {
		return "", "", fmt.Errorf("native Windows QEMU does not include virtio-9p; use --share none or set PVM_BACKEND=wsl for host shares")
	}
	return "none", "native Windows QEMU does not include virtio-9p; using --share none (set PVM_BACKEND=wsl for host shares)", nil
}

func shareFlagExplicit(args []string) bool {
	for _, arg := range args {
		if arg == "--share" || arg == "-share" || strings.HasPrefix(arg, "--share=") || strings.HasPrefix(arg, "-share=") {
			return true
		}
	}
	return false
}

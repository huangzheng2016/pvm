//go:build linux

package hostnet

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func parseDefaultRouteDev(routes string) string {
	for _, line := range strings.Split(routes, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[1] == "00000000" {
			return f[0]
		}
	}
	return ""
}

func hasPhysicalDevice(sysClassNet, dev string, depth int) bool {
	if depth > 8 || IsVirtual(dev) {
		return false
	}
	base := filepath.Join(sysClassNet, dev)
	if _, err := os.Stat(filepath.Join(base, "device")); err == nil {
		return true
	}
	matches, _ := filepath.Glob(filepath.Join(base, "lower_*"))
	for _, m := range matches {
		if real, err := filepath.EvalSymlinks(m); err == nil {
			if hasPhysicalDevice(sysClassNet, filepath.Base(real), depth+1) {
				return true
			}
		}
	}
	return false
}

func selectPhysical() (*Interface, error) {
	raw, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return nil, err
	}
	dev := parseDefaultRouteDev(string(raw))
	if dev == "" || !hasPhysicalDevice("/sys/class/net", dev, 0) {
		return nil, fmt.Errorf("default route interface %q is not a physical device", dev)
	}
	return &Interface{Name: dev}, nil
}

func bindControl(name string, _ int) func(string, string, syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		if skipBindForAddress(address) {
			return nil
		}
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, name)
			// SO_BINDTODEVICE requires CAP_NET_RAW on Linux; an unprivileged
			// host process must not lose connectivity just because of binding.
			if errors.Is(serr, syscall.EPERM) {
				serr = nil
			}
		}); err != nil {
			return err
		}
		return serr
	}
}

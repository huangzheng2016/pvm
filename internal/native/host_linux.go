//go:build linux

package native

import (
	"fmt"
	"os"
	"strings"
)

// HostMemMB returns the host's physical memory in MiB.
func HostMemMB() int {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 8192
	}
	for line := range strings.Lines(string(data)) {
		if rest, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			var kb int
			if _, err := fmt.Sscanf(strings.TrimSpace(rest), "%d", &kb); err == nil && kb > 0 {
				return kb / 1024
			}
		}
	}
	return 8192
}

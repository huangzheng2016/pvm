package agent

import (
	"fmt"
	"os"
	"path/filepath"
)

// Scan sysfs by port name because virtio device indices are not stable.
func FindVirtioPort(root, name string) (string, error) {
	dir := filepath.Join(root, "sys", "class", "virtio-ports")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("virtio-ports: %w", err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name(), "name"))
		if err != nil {
			continue
		}
		if string(trimNL(data)) == name {
			dev := filepath.Join(root, "dev", e.Name())
			if _, err := os.Stat(dev); err == nil {
				return dev, nil
			}
			return "", fmt.Errorf("port %s found at %s but %s missing", name, e.Name(), dev)
		}
	}
	return "", fmt.Errorf("virtio port %q not found under %s", name, dir)
}

func trimNL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

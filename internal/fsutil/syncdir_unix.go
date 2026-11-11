//go:build unix

package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
)

// syncDir syncs the directory containing the given path to ensure durability.
// On Unix, this ensures the directory entry is flushed to disk.
func syncDir(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open directory for sync: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}
	return nil
}

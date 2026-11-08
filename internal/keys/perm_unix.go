//go:build !windows

package keys

import (
	"fmt"
	"os"
	"syscall"
)

func checkPrivatePathOS(info os.FileInfo, kind string, directory bool) error {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("SSH %s is owned by another user; restore ownership before continuing", kind)
	}
	mask := os.FileMode(0o077)
	if directory {
		mask = 0o022
	}
	if info.Mode().Perm()&mask != 0 {
		return fmt.Errorf("SSH %s is accessible to other users; restrict permissions before continuing", kind)
	}
	return nil
}

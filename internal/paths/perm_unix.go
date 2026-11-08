//go:build !windows

package paths

import (
	"fmt"
	"os"
	"syscall"
)

func checkPrivateDirOS(info os.FileInfo) error {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("pvm state directory is owned by another user; restore ownership before continuing")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("pvm state directory is accessible to other users; restrict its permissions to 0700 before continuing")
	}
	return nil
}

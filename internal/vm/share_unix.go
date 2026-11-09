//go:build !windows

package vm

import "os"

func preparePrivateShare(path string, guestUID int) error {
	if os.Getuid() != 0 {
		return nil
	}
	return os.Chown(path, guestUID, -1)
}

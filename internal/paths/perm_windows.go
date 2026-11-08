//go:build windows

package paths

import "os"

func checkPrivateDirOS(info os.FileInfo) error {
	return nil
}

//go:build windows

package keys

import "os"

func checkPrivatePathOS(info os.FileInfo, kind string, directory bool) error {
	return nil
}

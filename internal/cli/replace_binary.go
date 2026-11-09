//go:build !windows

package cli

import "github.com/huangzheng2016/pvm/internal/fsutil"

func replaceBinary(path string, data []byte) error {
	return fsutil.AtomicWrite(path, data, 0o755)
}

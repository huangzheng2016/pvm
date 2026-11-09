//go:build windows

package vm

// Windows does not support fsync on directory handles through os.File.Sync.
// The seed file itself has already been flushed before its atomic rename.
func syncDirectory(path string) error {
	return nil
}

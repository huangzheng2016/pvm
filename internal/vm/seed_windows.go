//go:build windows

package vm

// syncDir is a no-op on Windows because:
// 1. Windows does not support Sync() on directory handles (returns "Access is denied")
// 2. Windows file system semantics differ - the file Sync() before Rename is sufficient
// 3. NTFS and ReFS provide atomic rename operations without explicit directory sync
//
// Reference: https://github.com/golang/go/issues/24015
func syncDir(path string) error {
	return nil
}

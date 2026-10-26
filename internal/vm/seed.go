package vm

import (
	"fmt"
	"os"
	"path/filepath"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
)

// WriteSeed uses FAT32 because diskfs hardcodes the ISO9660 volume label
// to ISOIMAGE, while NoCloud requires "cidata" and accepts vfat.
func WriteSeed(path string, userData, metaData []byte) (retErr error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	os.Remove(tmp)
	// 32 MiB is far above the NoCloud payload but small enough to stay
	// sparse-friendly on APFS/ext4.
	d, err := diskfs.Create(tmp, 32*1024*1024, diskfs.SectorSize(512))
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	closed := false
	defer func() {
		if !closed {
			if err := d.Close(); retErr == nil {
				retErr = err
			}
		}
	}()
	fs, err := d.CreateFilesystem(disk.FilesystemSpec{
		Partition:   0,
		FSType:      filesystem.TypeFat32,
		VolumeLabel: "cidata",
	})
	if err != nil {
		return err
	}
	for name, content := range map[string][]byte{
		"/user-data": userData,
		"/meta-data": metaData,
	} {
		f, err := fs.OpenFile(name, os.O_CREATE|os.O_RDWR)
		if err != nil {
			return fmt.Errorf("seed %s: %w", name, err)
		}
		if _, err := f.Write(content); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	if err := d.Close(); err != nil {
		closed = true
		return err
	}
	closed = true
	f, err := os.OpenFile(tmp, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("seed installed but opening directory for sync failed: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("seed installed but directory sync failed: %w", err)
	}
	return nil
}

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
func WriteSeed(path string, userData, metaData []byte) error {
	tmp := path + ".tmp"
	os.Remove(tmp)
	// 32 MiB is far above the NoCloud payload but small enough to stay
	// sparse-friendly on APFS/ext4.
	d, err := diskfs.Create(tmp, 32*1024*1024, diskfs.SectorSize(512))
	if err != nil {
		return err
	}
	defer os.Remove(tmp) // no-op once renamed
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
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// fsync the image (via a second handle, diskfs owns the first) and
	// the directory after rename, so a crash cannot yield a 0-byte seed
	// (the VM would boot with no user and no way in).
	if f, err := os.OpenFile(tmp, os.O_RDWR, 0); err == nil {
		f.Sync()
		f.Close()
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		dir.Sync()
		dir.Close()
	}
	return nil
}

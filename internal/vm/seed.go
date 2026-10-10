package vm

import (
	"fmt"
	"os"
	"path/filepath"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
)

// WriteSeed builds a cloud-init NoCloud seed: a FAT32 image labeled
// "cidata" holding user-data and meta-data. Chosen over ISO9660 because
// diskfs ignores the volume label for ISO9660 (hardcodes ISOIMAGE) and
// cloud-init NoCloud accepts vfat just the same. Generated purely in Go.
func WriteSeed(path string, userData, metaData []byte) error {
	tmp := path + ".tmp"
	os.Remove(tmp)
	// 32 MiB is far above the NoCloud payload but small enough to stay
	// sparse-friendly on APFS/ext4.
	d, err := diskfs.Create(tmp, 32*1024*1024, diskfs.SectorSize(512))
	if err != nil {
		return err
	}
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
	return os.Rename(tmp, path)
}

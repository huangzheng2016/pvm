package qemu

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

type CompactResult struct {
	BeforeBytes int64 `json:"before_bytes"`
	AfterBytes  int64 `json:"after_bytes"`
	Changed     bool  `json:"changed"`
}

type diskImageInfo struct {
	VirtualSize           int64  `json:"virtual-size"`
	ActualSize            int64  `json:"actual-size"`
	Format                string `json:"format"`
	BackingFilename       string `json:"backing-filename"`
	FullBackingFilename   string `json:"full-backing-filename"`
	BackingFilenameFormat string `json:"backing-filename-format"`
}

func CompactDisk(tc *Toolchain, path string) (*CompactResult, error) {
	if tc == nil || tc.Img == "" {
		return nil, fmt.Errorf("qemu-img is unavailable")
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("disk %s is not a regular file", path)
	}
	before, err := readDiskImageInfo(tc, path)
	if err != nil {
		return nil, err
	}
	if before.Format != "qcow2" {
		return nil, fmt.Errorf("disk %s has format %q, want qcow2", path, before.Format)
	}
	dir := filepath.Dir(path)
	tmp, err := temporaryDiskName(dir, ".compact-*.qcow2")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp)
	args := []string{"convert", "-f", "qcow2", "-O", "qcow2", "-S", "4k"}
	backing := canonicalBacking(path, before)
	if backing != "" {
		if before.BackingFilenameFormat == "" {
			return nil, fmt.Errorf("disk %s has an unknown backing format", path)
		}
		args = append(args, "-b", backing, "-F", before.BackingFilenameFormat)
	}
	args = append(args, path, tmp)
	if out, err := exec.Command(tc.Img, args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("qemu-img convert: %v\n%s", err, out)
	}
	if out, err := exec.Command(tc.Img, "check", "-f", "qcow2", tmp).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("qemu-img check: %v\n%s", err, out)
	}
	after, err := readDiskImageInfo(tc, tmp)
	if err != nil {
		return nil, err
	}
	if after.Format != "qcow2" || after.VirtualSize != before.VirtualSize {
		return nil, fmt.Errorf("compacted disk validation failed: format or virtual size changed")
	}
	if canonicalBacking(tmp, after) != backing {
		return nil, fmt.Errorf("compacted disk validation failed: backing file changed")
	}
	result := &CompactResult{BeforeBytes: before.ActualSize, AfterBytes: after.ActualSize}
	if after.ActualSize >= before.ActualSize {
		return result, nil
	}
	if err := os.Chmod(tmp, fi.Mode().Perm()); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(tmp, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	backup, err := temporaryDiskName(dir, ".compact-backup-*.qcow2")
	if err != nil {
		return nil, err
	}
	defer os.Remove(backup)
	if err := os.Link(path, backup); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, err
	}
	if err := os.Remove(backup); err != nil {
		return nil, fmt.Errorf("disk compacted but old backup %s could not be removed: %w", backup, err)
	}
	result.Changed = true
	return result, nil
}

func readDiskImageInfo(tc *Toolchain, path string) (*diskImageInfo, error) {
	out, err := exec.Command(tc.Img, "info", "--output=json", path).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("qemu-img info: %v\n%s", err, out)
	}
	var info diskImageInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return nil, fmt.Errorf("qemu-img info for %s: %w", path, err)
	}
	return &info, nil
}

func canonicalBacking(path string, info *diskImageInfo) string {
	backing := info.FullBackingFilename
	if backing == "" {
		backing = info.BackingFilename
	}
	if backing == "" {
		return ""
	}
	if !filepath.IsAbs(backing) {
		backing = filepath.Join(filepath.Dir(path), backing)
	}
	return filepath.Clean(backing)
}

func temporaryDiskName(dir, pattern string) (string, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	if err := os.Remove(path); err != nil {
		return "", err
	}
	return path, nil
}

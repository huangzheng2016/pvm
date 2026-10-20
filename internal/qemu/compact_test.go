package qemu

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func requireCompactTools(t *testing.T) (string, string) {
	t.Helper()
	img, err := exec.LookPath("qemu-img")
	if err != nil {
		t.Skip("qemu-img not installed")
	}
	io, err := exec.LookPath("qemu-io")
	if err != nil {
		t.Skip("qemu-io not installed")
	}
	return img, io
}

func runCompactTool(t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func TestCompactDiskReclaimsSpace(t *testing.T) {
	img, io := requireCompactTools(t)
	disk := filepath.Join(t.TempDir(), "disk.qcow2")
	runCompactTool(t, img, "create", "-f", "qcow2", disk, "64M")
	runCompactTool(t, io, "-f", "qcow2",
		"-c", "write -P 0x5a 0 16M",
		"-c", "write -P 0x00 0 16M",
		disk,
	)
	tc := &Toolchain{Img: img}
	before, err := readDiskImageInfo(tc, disk)
	if err != nil {
		t.Fatal(err)
	}
	if before.ActualSize < 4<<20 {
		t.Fatalf("test setup did not allocate historical clusters: %d", before.ActualSize)
	}
	result, err := CompactDisk(tc, disk)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed {
		t.Fatal("historical zero clusters were not reclaimed")
	}
	if result.AfterBytes >= result.BeforeBytes {
		t.Fatalf("size did not shrink: %+v", result)
	}
	after, err := readDiskImageInfo(tc, disk)
	if err != nil {
		t.Fatal(err)
	}
	if after.VirtualSize != 64<<20 {
		t.Fatalf("virtual size changed to %d", after.VirtualSize)
	}
}

func TestCompactDiskPreservesBacking(t *testing.T) {
	img, _ := requireCompactTools(t)
	dir := t.TempDir()
	base := filepath.Join(dir, "base.qcow2")
	disk := filepath.Join(dir, "disk.qcow2")
	runCompactTool(t, img, "create", "-f", "qcow2", base, "32M")
	runCompactTool(t, img, "create", "-f", "qcow2", "-b", base, "-F", "qcow2", disk)
	tc := &Toolchain{Img: img}
	if _, err := CompactDisk(tc, disk); err != nil {
		t.Fatal(err)
	}
	after, err := readDiskImageInfo(tc, disk)
	if err != nil {
		t.Fatal(err)
	}
	if after.VirtualSize != 32<<20 {
		t.Fatalf("virtual size changed to %d", after.VirtualSize)
	}
	if got := canonicalBacking(disk, after); got != base {
		t.Fatalf("backing = %q, want %q", got, base)
	}
}

func TestCompactDiskRejectsRaw(t *testing.T) {
	img, _ := requireCompactTools(t)
	disk := filepath.Join(t.TempDir(), "disk.raw")
	if err := os.WriteFile(disk, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := CompactDisk(&Toolchain{Img: img}, disk)
	if err == nil || !strings.Contains(err.Error(), "want qcow2") {
		t.Fatalf("raw disk error = %v", err)
	}
}

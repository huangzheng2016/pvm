// Package qemu requires a complete native toolchain: missing qemu-img is
// fatal because silently booting without a disk is unsafe.
package qemu

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/huangzheng2016/pvm/internal/fsutil"
	"github.com/huangzheng2016/pvm/internal/native"
)

type Toolchain struct {
	System      string // qemu-system-<arch>
	Img         string // qemu-img
	FirmwareDir string // dir containing edk2-*.fd
	Bundled     bool
	Version     [3]int // e.g. {10,0,3}
	has9p       *bool
}

// probeVersion runs `qemu-system --version` and parses "QEMU emulator
// version X.Y.Z". A binary that cannot even start (e.g. missing dylibs on
// a brew-less machine) yields {0,0,0} and is treated as unusable.
func probeVersion(system string) [3]int {
	cmd := exec.Command(system, "--version")
	fsutil.ConfigureHiddenCmd(cmd)
	out, err := cmd.Output()
	if err != nil {
		return [3]int{}
	}
	var v [3]int
	if _, err := fmt.Sscanf(string(out), "QEMU emulator version %d.%d.%d", &v[0], &v[1], &v[2]); err != nil {
		return [3]int{}
	}
	return v
}

// SupportsVirtio9p reports whether the QEMU binary supports virtio-9p-pci.
// Windows QEMU distributions frequently omit 9p support.
func (t *Toolchain) SupportsVirtio9p() bool {
	if t == nil || t.System == "" || runtime.GOOS != "windows" {
		return true
	}
	if t.has9p != nil {
		return *t.has9p
	}
	cmd := exec.Command(t.System, "-device", "help")
	fsutil.ConfigureHiddenCmd(cmd)
	out, err := cmd.CombinedOutput()
	ok := err == nil && strings.Contains(string(out), "virtio-9p-pci")
	t.has9p = &ok
	return ok
}

// BrokenLiveRestore refuses QEMU 11.0.0 and unknown versions on darwin/arm64
// HVF: 11.0.0 asserts on incoming migration (fixed in 11.1.0), and restoring
// on an unknown runtime is riskier than rejecting it. Bundled runtimes are
// known-good and exempt.
func (t *Toolchain) BrokenLiveRestore(accel Accel) bool {
	if t.Bundled || native.Arch != "arm64" || accel != AccelHVF {
		return false
	}
	return t.Version == [3]int{11, 0, 0} || t.Version == [3]int{0, 0, 0}
}

func QemuArch() string { return native.QemuArch() }

func exeSuffix(name string) string {
	if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(name), ".exe") {
		return name + ".exe"
	}
	return name
}

func Find(runtimeDir string) (*Toolchain, error) {
	arch := QemuArch()
	binName := exeSuffix("qemu-system-" + arch)
	imgName := exeSuffix("qemu-img")

	bundledBin := filepath.Join(runtimeDir, "qemu", arch, "bin")
	if t, err := lookup(filepath.Join(bundledBin, binName), filepath.Join(bundledBin, imgName)); err == nil && t.Version != [3]int{0, 0, 0} {
		t.Bundled = true
		return t, nil
	}

	var prefixes []string
	if p, err := exec.LookPath(binName); err == nil {
		prefixes = append(prefixes, filepath.Dir(p))
	}
	switch runtime.GOOS {
	case "windows":
		for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)"} {
			if base := os.Getenv(env); base != "" {
				prefixes = append(prefixes, filepath.Join(base, "qemu"), filepath.Join(base, "QEMU"))
			}
		}
		if local := os.Getenv("LocalAppData"); local != "" {
			prefixes = append(prefixes, filepath.Join(local, "Programs", "qemu"))
		}
	default:
		prefixes = append(prefixes, "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin")
	}

	for _, dir := range prefixes {
		if t, err := lookup(filepath.Join(dir, binName), filepath.Join(dir, imgName)); err == nil {
			return t, nil
		}
	}
	return nil, fmt.Errorf("qemu-system-%s (and qemu-img) not found; install qemu (e.g. brew install qemu)", arch)
}

func lookup(system, img string) (*Toolchain, error) {
	if !executable(system) || !executable(img) {
		return nil, fmt.Errorf("not found")
	}
	// Homebrew layout: bin/../share/qemu holds edk2 firmware; Windows NSIS uses share/.
	fw := filepath.Join(filepath.Dir(system), "..", "share", "qemu")
	if fi, err := os.Stat(fw); err != nil || !fi.IsDir() {
		alt := filepath.Join(filepath.Dir(system), "share")
		if fi2, err2 := os.Stat(alt); err2 == nil && fi2.IsDir() {
			fw = alt
		} else {
			fw = ""
		}
	}
	return &Toolchain{System: system, Img: img, FirmwareDir: fw, Version: probeVersion(system)}, nil
}

func executable(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return fi.Mode()&0o111 != 0
}

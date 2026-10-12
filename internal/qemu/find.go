// Package qemu locates the QEMU toolchain and probes accelerators.
// Search order: bundled under ~/.pvm/runtime/qemu first, then PATH, then
// well-known install prefixes. Missing qemu-img is a hard error (the
// platform bug of silently booting diskless is not repeated here).
package qemu

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/huangzheng2016/pvm/internal/native"
)

type Toolchain struct {
	System      string // qemu-system-<arch>
	Img         string // qemu-img
	FirmwareDir string // dir containing edk2-*.fd
	Bundled     bool
	Version     [3]int // e.g. {10,0,3}
}

// probeVersion runs `qemu-system --version` and parses "QEMU emulator
// version X.Y.Z". A binary that cannot even start (e.g. missing dylibs on
// a brew-less machine) yields {0,0,0} and is treated as unusable.
func probeVersion(system string) [3]int {
	out, err := exec.Command(system, "--version").Output()
	if err != nil {
		return [3]int{}
	}
	var v [3]int
	if _, err := fmt.Sscanf(string(out), "QEMU emulator version %d.%d.%d", &v[0], &v[1], &v[2]); err != nil {
		return [3]int{}
	}
	return v
}

// BrokenLiveRestore reports the QEMU 11.0.0 darwin/arm64 HVF regression
// (cpu_pre_load assertion on incoming migration, fixed upstream by
// 06fd39e426 which first ships in 11.1.0). Only the exact 11.0.0
// release is broken; 11.1+ stock builds pass, and bundled runtimes are
// pvm's own known-good builds and are always exempt. An unparseable
// version ({0,0,0}) is treated as unknown and refused too — the restore
// crash is worse than a false positive.
func (t *Toolchain) BrokenLiveRestore(accel Accel) bool {
	if t.Bundled || native.Arch != "arm64" || accel != AccelHVF {
		return false
	}
	return t.Version == [3]int{11, 0, 0} || t.Version == [3]int{0, 0, 0}
}

// QemuArch returns the native QEMU system target.
func QemuArch() string { return native.QemuArch() }

// Find locates the native qemu-system and qemu-img.
func Find(runtimeDir string) (*Toolchain, error) {
	arch := QemuArch()
	binName := "qemu-system-" + arch

	bundledBin := filepath.Join(runtimeDir, "qemu", arch, "bin")
	if t, err := lookup(filepath.Join(bundledBin, binName), filepath.Join(bundledBin, "qemu-img")); err == nil && t.Version != [3]int{0, 0, 0} {
		t.Bundled = true
		return t, nil
	}

	var prefixes []string
	if p, err := exec.LookPath(binName); err == nil {
		prefixes = append(prefixes, filepath.Dir(p))
	}
	prefixes = append(prefixes, "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin")

	for _, dir := range prefixes {
		if t, err := lookup(filepath.Join(dir, binName), filepath.Join(dir, "qemu-img")); err == nil {
			return t, nil
		}
	}
	return nil, fmt.Errorf("qemu-system-%s (and qemu-img) not found; install qemu (e.g. brew install qemu)", arch)
}

func lookup(system, img string) (*Toolchain, error) {
	if !executable(system) || !executable(img) {
		return nil, fmt.Errorf("not found")
	}
	// Homebrew layout: bin/../share/qemu holds edk2 firmware.
	fw := filepath.Join(filepath.Dir(system), "..", "share", "qemu")
	if fi, err := os.Stat(fw); err != nil || !fi.IsDir() {
		fw = ""
	}
	return &Toolchain{System: system, Img: img, FirmwareDir: fw, Version: probeVersion(system)}, nil
}

func executable(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0
}

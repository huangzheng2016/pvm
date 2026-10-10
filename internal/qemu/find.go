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
)

type Toolchain struct {
	System      string // qemu-system-<arch>
	Img         string // qemu-img
	FirmwareDir string // dir containing edk2-*.fd
	Bundled     bool
}

// QemuArch maps GOARCH to the qemu-system suffix.
func QemuArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	default:
		return goarch
	}
}

// Find locates qemu-system-<goarch> and qemu-img for the given GOARCH.
func Find(runtimeDir, goarch string) (*Toolchain, error) {
	arch := QemuArch(goarch)
	binName := "qemu-system-" + arch

	bundledBin := filepath.Join(runtimeDir, "qemu", arch, "bin")
	if t, err := lookup(filepath.Join(bundledBin, binName), filepath.Join(bundledBin, "qemu-img")); err == nil {
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
	return nil, fmt.Errorf("qemu-system-%s (and qemu-img) not found; install qemu or run `pvm runtime fetch` (M2)", arch)
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
	return &Toolchain{System: system, Img: img, FirmwareDir: fw}, nil
}

func executable(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0
}

package vm

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/huangzheng2016/pvm/internal/native"
	"github.com/huangzheng2016/pvm/internal/paths"
	"github.com/huangzheng2016/pvm/internal/qemu"
)

// CloneSpec derives the spec for a clone of src. Hardware/profile fields
// follow the source; identity fields (IP/MAC, region-route state) are
// fresh, and IdentityReset is set so the first boot regenerates guest
// hostname/machine-id/ssh host keys.
func CloneSpec(src *Spec, newName, ip, mac string) *Spec {
	dst := *src
	dst.Name = newName
	dst.CreatedAt = time.Now()
	dst.Role = "user"
	dst.IP = ip
	dst.MAC = mac
	dst.PrevTZ = ""
	dst.PrevLocale = ""
	dst.IdentityReset = true
	if src.Mounts != nil {
		dst.Mounts = append([]GuestMount(nil), src.Mounts...)
	}
	return &dst
}

// CloneDisk flattens the source disk so the clone has no backing chain.
// The copied seed keeps its instance-id; identity reset happens over SSH
// after boot. The source must be stopped because converting a live disk is unsafe.
// created reports whether this call owns the destination directory.
func CloneDisk(p *paths.Paths, tc *qemu.Toolchain, src, dst *Spec) (created bool, err error) {
	dir := filepath.Join(p.VMs, dst.Name)
	sourceDisk := src.Disk(p)
	restore := qemu.SnapPaths(p.VMDir(src.Name)).Restore
	if _, err := os.Stat(restore); err == nil {
		sourceDisk = restore
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("inspect source restore disk: %w", err)
	}
	if _, err := os.Stat(sourceDisk); err != nil {
		return false, fmt.Errorf("source disk for %s not found: %w", src.Name, err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return false, err
	}
	if dst.ShareMode == SharePrivate {
		if err := os.Mkdir(p.VMShare(dst.Name), 0o755); err != nil {
			return true, err
		}
	}
	out, err := exec.Command(tc.Img, "convert", "-f", "qcow2", "-O", "qcow2", sourceDisk, dst.Disk(p)).CombinedOutput()
	if err != nil {
		return true, fmt.Errorf("qemu-img convert: %v\n%s", err, out)
	}
	if native.Arch == "arm64" {
		// Blank 64 MiB pflash; OVMF formats it on first boot.
		if err := os.WriteFile(dst.Vars(p), make([]byte, 64*1024*1024), 0o600); err != nil {
			return true, err
		}
	}
	seed, err := os.ReadFile(src.Seed(p))
	if err != nil {
		return true, fmt.Errorf("read source seed: %w", err)
	}
	if err := os.WriteFile(dst.Seed(p), seed, 0o600); err != nil {
		return true, err
	}
	return true, nil
}

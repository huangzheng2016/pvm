package vm

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

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
func CloneDisk(p *paths.Paths, tc *qemu.Toolchain, src, dst *Spec) error {
	dir := filepath.Join(p.VMs, dst.Name)
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("vm %q already exists", dst.Name)
	}
	sourceDisk := src.Disk(p)
	restore := qemu.SnapPaths(p.VMDir(src.Name)).Restore
	if _, err := os.Stat(restore); err == nil {
		sourceDisk = restore
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect source restore disk: %w", err)
	}
	if _, err := os.Stat(sourceDisk); err != nil {
		return fmt.Errorf("source disk for %s not found: %w", src.Name, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	out, err := exec.Command(tc.Img, "convert", "-f", "qcow2", "-O", "qcow2", sourceDisk, dst.Disk(p)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img convert: %v\n%s", err, out)
	}
	if native.Arch == "arm64" {
		// Blank 64 MiB pflash; OVMF formats it on first boot.
		if err := os.WriteFile(dst.Vars(p), make([]byte, 64*1024*1024), 0o600); err != nil {
			return err
		}
	}
	seed, err := os.ReadFile(src.Seed(p))
	if err != nil {
		return fmt.Errorf("read source seed: %w", err)
	}
	if err := os.WriteFile(dst.Seed(p), seed, 0o600); err != nil {
		return err
	}
	return nil
}

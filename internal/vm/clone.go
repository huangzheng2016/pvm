package vm

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

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
	dst.Role = "user" // clones of user vms stay user vms
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

// CloneDisk creates the destination VM directory with a full copy of the
// source disk (no backing chain), fresh UEFI vars, and a copy of the
// source seed (same user/mounts; cloud-init sees the same instance-id
// and does not re-run — identity reset happens over ssh after boot).
// The source VM must be stopped: converting a live disk is unsafe.
func CloneDisk(p *paths.Paths, tc *qemu.Toolchain, src, dst *Spec) error {
	dir := filepath.Join(p.VMs, dst.Name)
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("vm %q already exists", dst.Name)
	}
	if _, err := os.Stat(src.Disk(p)); err != nil {
		return fmt.Errorf("source disk for %s not found: %w", src.Name, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	out, err := exec.Command(tc.Img, "convert", "-O", "qcow2", src.Disk(p), dst.Disk(p)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img convert: %v\n%s", err, out)
	}
	if dst.Arch == "arm64" {
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
